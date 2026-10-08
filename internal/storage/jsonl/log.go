package jsonl

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/rexzhao/simple-agent/internal/storage"
)

// Log is the append-only JSONL file of one session. It owns the file and
// the durability policy. Append is safe for concurrent use; readers go
// through Records.
type Log struct {
	path string

	mu     sync.Mutex
	file   *os.File
	writer *bufio.Writer
	closed bool
}

// OpenLog opens (creating if needed) the log at path, recovers a torn tail
// left by a crash mid-append, and returns the log positioned for appends.
func OpenLog(path string) (*Log, error) {
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return nil, fmt.Errorf("create session directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open session log: %w", err)
	}
	log := &Log{path: path, file: file, writer: bufio.NewWriter(file)}
	if err := log.recover(); err != nil {
		file.Close()
		return nil, err
	}
	return log, nil
}

// recover scans the file line by line and truncates a torn tail: a final
// line without its newline, or one that is not valid JSON for a valid
// record.
//
// The write path guarantees a torn line can only be the last one: appends
// are strictly ordered by the log mutex, every append flushes its complete
// line, and an fsync of a durable record also solidifies every earlier
// write. A mid-file invalid line therefore indicates external corruption
// or a manual edit; recovery still truncates from there (fail-safe over
// fail-stop), and reports how much was discarded.
//
// Line order is the record order: a single writer appends under the mutex,
// so no content-level ordering check is performed (external tools
// inserting or reordering valid lines are outside the threat model).
func (l *Log) recover() error {
	if _, err := l.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek session log: %w", err)
	}
	reader := bufio.NewReader(l.file)
	var offset int64
	var validLines int
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if line[len(line)-1] != '\n' {
				// Torn tail: the writer crashed before completing the line.
				break
			}
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 {
				offset += int64(len(line))
				continue
			}
			var record storage.Record
			if decodeErr := json.Unmarshal(trimmed, &record); decodeErr != nil || record.Validate() != nil {
				// An invalid complete line is treated like a torn tail: under
				// the write-path invariant it can only be external corruption
				// (see the comment on recover), and truncating from the first
				// invalid line is the fail-safe choice.
				break
			}
			validLines++
			offset += int64(len(line))
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("read session log: %w", err)
		}
	}
	info, err := l.file.Stat()
	if err != nil {
		return fmt.Errorf("stat session log: %w", err)
	}
	if info.Size() > offset {
		if err := l.file.Truncate(offset); err != nil {
			return fmt.Errorf("truncate torn session log tail: %w", err)
		}
		if info.Size()-offset > maxTornTailBytes {
			fmt.Fprintf(os.Stderr, "jsonl: %s: discarded %d bytes after %d valid records; mid-file corruption, not a torn tail\n",
				l.path, info.Size()-offset, validLines)
		}
	}
	// Truncation does not move the file offset, and the reading pass left it
	// at the old end of file. Reposition exactly at the recovered boundary so
	// the next append extends the last valid line instead of sparse-writing
	// zero padding beyond the truncation point.
	if _, err := l.file.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("seek session log after recovery: %w", err)
	}
	return nil
}

// Append fills the record's time, writes it as one line, and fsyncs when
// the record class requires durability. It returns the stored record.
func (l *Log) Append(record storage.Record) (storage.Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return storage.Record{}, fmt.Errorf("session log is closed")
	}
	if record.Time == "" {
		record.Time = storage.NewTime()
	}
	if err := record.Validate(); err != nil {
		return storage.Record{}, err
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return storage.Record{}, fmt.Errorf("encode record: %w", err)
	}
	if len(encoded) > storage.MaxRecordBytes {
		return storage.Record{}, fmt.Errorf("record of type %s is %d bytes, over the %d byte limit", record.Type, len(encoded), storage.MaxRecordBytes)
	}
	line := append(encoded, '\n')
	if _, err := l.writer.Write(line); err != nil {
		return storage.Record{}, fmt.Errorf("append record: %w", err)
	}
	if err := l.writer.Flush(); err != nil {
		return storage.Record{}, fmt.Errorf("flush record: %w", err)
	}
	if record.Durable() {
		if err := l.file.Sync(); err != nil {
			return storage.Record{}, fmt.Errorf("sync record: %w", err)
		}
	}
	return record, nil
}

// Records returns every stored record in sequence order.
func (l *Log) Records() ([]storage.Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, fmt.Errorf("session log is closed")
	}
	if err := l.writer.Flush(); err != nil {
		return nil, fmt.Errorf("flush session log: %w", err)
	}
	return readRecords(l.path)
}

// Close flushes and closes the file. It is idempotent.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	err := l.writer.Flush()
	if syncErr := l.file.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := l.file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func readRecords(path string) ([]storage.Record, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open session log: %w", err)
	}
	defer file.Close()
	var records []storage.Record
	reader := bufio.NewReaderSize(file, 64*1024)
	for {
		line, err := reader.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			var record storage.Record
			if decodeErr := json.Unmarshal(trimmed, &record); decodeErr != nil || record.Validate() != nil {
				return nil, fmt.Errorf("session log %s has an invalid line", path)
			}
			records = append(records, record)
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("read session log: %w", err)
		}
	}
	return records, nil
}

// maxTornTailBytes is the largest discard a crash mid-append can produce:
// one buffered record just above the frame limit. Recovering more than this
// means the file was damaged by something other than a torn append.
const maxTornTailBytes = 2 * storage.MaxRecordBytes

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[:i]
		}
	}
	return "."
}
