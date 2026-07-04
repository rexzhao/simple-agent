package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	defaultRegistryDirName  = "sai"
	registrySubdirName      = "server"
	defaultRegistryFileName = "registry.json"
	registryTokenBytes      = 32
)

// RegistryRecord describes the namespace singleton server in the per-user registry.
// BaseURL currently stores the existing host:port value used by local clients;
// only the registry JSON field name is base_url in this slice.
type RegistryRecord struct {
	CWD             string    `json:"cwd"`
	ConfigPath      string    `json:"config_path"`
	BaseURL         string    `json:"base_url"`
	PID             int       `json:"pid"`
	Token           string    `json:"token"`
	StartedAt       time.Time `json:"started_at"`
	Version         string    `json:"version"`
	RequestedListen string    `json:"requested_listen,omitempty"`
}

// RegistryIdentity is the canonical cwd + config path pair for one server.
type RegistryIdentity struct {
	CWD        string
	ConfigPath string
}

// RegistryStore reads and writes the local server registry file.
type RegistryStore struct {
	Path string
}

// NewRegistryStore returns a registry store. An empty path uses DefaultRegistryPath.
func NewRegistryStore(path string) RegistryStore {
	return RegistryStore{Path: path}
}

// DefaultRegistryPath returns the per-user server registry file path.
func DefaultRegistryPath() (string, error) {
	home, err := DefaultHomeDir()
	if err != nil {
		return "", err
	}
	return RegistryPathForHome(home)
}

// DefaultHomeDir returns the built-in user-level home namespace directory.
func DefaultHomeDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user config dir: %w", err)
	}
	return filepath.Join(dir, defaultRegistryDirName), nil
}

// RegistryPathForHome returns the singleton registry file path for a home namespace.
func RegistryPathForHome(home string) (string, error) {
	home, err := CanonicalPath(home)
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, registrySubdirName, defaultRegistryFileName), nil
}

// NewRegistryStoreForHome returns a registry store rooted in a home namespace.
func NewRegistryStoreForHome(home string) (RegistryStore, error) {
	path, err := RegistryPathForHome(home)
	if err != nil {
		return RegistryStore{}, err
	}
	return NewRegistryStore(path), nil
}

// HomeEnvVarName derives the home override environment variable from raw argv[0].
func HomeEnvVarName(argv0 string) string {
	base := filepath.Base(strings.TrimSpace(argv0))
	if ext := filepath.Ext(base); strings.EqualFold(ext, ".exe") {
		base = base[:len(base)-len(ext)]
	}
	base = strings.ToUpper(base)

	var out strings.Builder
	previousUnderscore := false
	for _, r := range base {
		isASCIIAlnum := r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !isASCIIAlnum {
			if !previousUnderscore {
				out.WriteByte('_')
				previousUnderscore = true
			}
			continue
		}
		out.WriteRune(r)
		previousUnderscore = false
	}
	normalized := strings.Trim(out.String(), "_")
	if normalized == "" {
		return ""
	}
	return normalized + "_HOME"
}

// ResolveHomeDir applies --home, derived env var, then the built-in default.
func ResolveHomeDir(argv0, explicitHome string) (string, error) {
	if strings.TrimSpace(explicitHome) != "" {
		return CanonicalPath(explicitHome)
	}
	if envName := HomeEnvVarName(argv0); envName != "" {
		if value := strings.TrimSpace(os.Getenv(envName)); value != "" {
			return CanonicalPath(value)
		}
	}
	return DefaultHomeDir()
}

// RegistryPath returns the store path, applying the default when Path is empty.
func (s RegistryStore) RegistryPath() (string, error) {
	if strings.TrimSpace(s.Path) == "" {
		return DefaultRegistryPath()
	}
	return CanonicalPath(s.Path)
}

// CanonicalPath returns an absolute, clean path without resolving symlinks.
func CanonicalPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}
	return filepath.Clean(abs), nil
}

// NewRegistryIdentity canonicalizes cwd and configPath into a server identity.
func NewRegistryIdentity(cwd, configPath string) (RegistryIdentity, error) {
	canonicalCWD, err := CanonicalPath(cwd)
	if err != nil {
		return RegistryIdentity{}, fmt.Errorf("canonicalize cwd: %w", err)
	}
	canonicalConfig, err := CanonicalPath(configPath)
	if err != nil {
		return RegistryIdentity{}, fmt.Errorf("canonicalize config path: %w", err)
	}
	return RegistryIdentity{
		CWD:        canonicalCWD,
		ConfigPath: canonicalConfig,
	}, nil
}

// Identity returns the record's cwd + config path identity.
func (r RegistryRecord) Identity() RegistryIdentity {
	return RegistryIdentity{
		CWD:        r.CWD,
		ConfigPath: r.ConfigPath,
	}
}

// Matches reports whether record has this exact canonical identity.
func (id RegistryIdentity) Matches(record RegistryRecord) bool {
	return sameRegistryPath(id.CWD, record.CWD) && sameRegistryPath(id.ConfigPath, record.ConfigPath)
}

// SameIdentity reports whether two normalized records describe the same server.
func (r RegistryRecord) SameIdentity(other RegistryRecord) bool {
	return r.Identity().Matches(other)
}

// SameRegistryIdentity reports whether two records have the same canonical identity.
func SameRegistryIdentity(a, b RegistryRecord) (bool, error) {
	a, err := CanonicalizeRegistryRecord(a)
	if err != nil {
		return false, err
	}
	b, err = CanonicalizeRegistryRecord(b)
	if err != nil {
		return false, err
	}
	return a.SameIdentity(b), nil
}

// CanonicalizeRegistryRecord returns a copy with canonical identity paths.
func CanonicalizeRegistryRecord(record RegistryRecord) (RegistryRecord, error) {
	identity, err := NewRegistryIdentity(record.CWD, record.ConfigPath)
	if err != nil {
		return RegistryRecord{}, err
	}
	record.CWD = identity.CWD
	record.ConfigPath = identity.ConfigPath
	record.BaseURL = strings.TrimSpace(record.BaseURL)
	record.Token = strings.TrimSpace(record.Token)
	record.Version = strings.TrimSpace(record.Version)
	record.RequestedListen = strings.TrimSpace(record.RequestedListen)
	if !record.StartedAt.IsZero() {
		record.StartedAt = record.StartedAt.UTC()
	}
	return record, nil
}

// GenerateRegistryToken creates a random local bearer token for registry clients.
func GenerateRegistryToken() (string, error) {
	var raw [registryTokenBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate registry token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// Load reads the registry file. A missing file loads as an empty registry.
func (s RegistryStore) Load() ([]RegistryRecord, error) {
	path, err := s.RegistryPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []RegistryRecord{}, nil
		}
		return nil, fmt.Errorf("read server registry %q: %w", path, err)
	}

	var records []RegistryRecord
	if err := json.Unmarshal(data, &records); err == nil {
		if records == nil {
			return []RegistryRecord{}, nil
		}
		return copyRegistryRecords(records), nil
	}

	var record RegistryRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("parse server registry %q: %w", path, err)
	}
	if record == (RegistryRecord{}) {
		return nil, fmt.Errorf("parse server registry %q: missing registry record fields", path)
	}
	return []RegistryRecord{record}, nil
}

// List returns all records in registry file order.
func (s RegistryStore) List() ([]RegistryRecord, error) {
	return s.Load()
}

// Save writes all records to the registry file using a temp file and rename.
func (s RegistryStore) Save(records []RegistryRecord) error {
	path, err := s.RegistryPath()
	if err != nil {
		return err
	}
	normalized, err := normalizeRegistryRecords(records)
	if err != nil {
		return err
	}
	if normalized == nil {
		normalized = []RegistryRecord{}
	}

	var payload any = []RegistryRecord{}
	if len(normalized) > 0 {
		payload = normalized[len(normalized)-1]
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("encode server registry: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create server registry dir: %w", err)
	}
	return writePrivateFileAtomic(path, data)
}

// Upsert replaces the active singleton record for this store.
func (s RegistryStore) Upsert(record RegistryRecord) error {
	normalized, err := CanonicalizeRegistryRecord(record)
	if err != nil {
		return err
	}
	return s.Save([]RegistryRecord{normalized})
}

// Remove deletes all records matching cwd + configPath.
func (s RegistryStore) Remove(cwd, configPath string) (bool, error) {
	identity, err := NewRegistryIdentity(cwd, configPath)
	if err != nil {
		return false, err
	}
	return s.RemoveIdentity(identity)
}

// RemoveIdentity deletes all records matching identity.
func (s RegistryStore) RemoveIdentity(identity RegistryIdentity) (bool, error) {
	records, err := s.Load()
	if err != nil {
		return false, err
	}

	out := make([]RegistryRecord, 0, len(records))
	removed := false
	for i, existing := range records {
		existing, err = CanonicalizeRegistryRecord(existing)
		if err != nil {
			return false, fmt.Errorf("canonicalize existing registry record %d: %w", i, err)
		}
		if identity.Matches(existing) {
			removed = true
			continue
		}
		out = append(out, existing)
	}
	if !removed {
		return false, nil
	}
	return true, s.Save(out)
}

// AncestorCWDs returns startCWD followed by each parent directory up to the root.
func AncestorCWDs(startCWD string) ([]string, error) {
	current, err := CanonicalPath(startCWD)
	if err != nil {
		return nil, err
	}
	ancestors := []string{current}
	for {
		parent := filepath.Dir(current)
		if parent == current {
			return ancestors, nil
		}
		current = parent
		ancestors = append(ancestors, current)
	}
}

// AncestorRecords returns records whose cwd is startCWD or one of its parents.
func AncestorRecords(startCWD string, records []RegistryRecord) ([]RegistryRecord, error) {
	ancestors, err := AncestorCWDs(startCWD)
	if err != nil {
		return nil, err
	}
	normalized, err := normalizeRegistryRecords(records)
	if err != nil {
		return nil, err
	}

	out := make([]RegistryRecord, 0, len(normalized))
	for _, ancestor := range ancestors {
		for _, record := range normalized {
			if sameRegistryPath(record.CWD, ancestor) {
				out = append(out, record)
			}
		}
	}
	return out, nil
}

// NearestAncestorRecord returns the closest record by cwd ancestry.
func NearestAncestorRecord(startCWD string, records []RegistryRecord) (RegistryRecord, bool, error) {
	matches, err := AncestorRecords(startCWD, records)
	if err != nil {
		return RegistryRecord{}, false, err
	}
	if len(matches) == 0 {
		return RegistryRecord{}, false, nil
	}
	return matches[0], true, nil
}

func normalizeRegistryRecords(records []RegistryRecord) ([]RegistryRecord, error) {
	if records == nil {
		return nil, nil
	}
	out := make([]RegistryRecord, 0, len(records))
	for i, record := range records {
		normalized, err := CanonicalizeRegistryRecord(record)
		if err != nil {
			return nil, fmt.Errorf("canonicalize registry record %d: %w", i, err)
		}
		out = append(out, normalized)
	}
	return out, nil
}

func copyRegistryRecords(records []RegistryRecord) []RegistryRecord {
	if records == nil {
		return nil
	}
	return append([]RegistryRecord(nil), records...)
}

func sameRegistryPath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func writePrivateFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".servers-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary server registry file: %w", err)
	}
	tempPath := temp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()

	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("chmod temporary server registry file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write temporary server registry file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary server registry file: %w", err)
	}
	if err := os.Chmod(tempPath, 0o600); err != nil {
		return fmt.Errorf("chmod temporary server registry file: %w", err)
	}

	if err := replacePrivateFile(tempPath, path); err != nil {
		return fmt.Errorf("write server registry %q: %w", path, err)
	}
	cleanup = false
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod server registry %q: %w", path, err)
	}
	return nil
}

func replacePrivateFile(tempPath, path string) error {
	if runtime.GOOS != "windows" {
		return os.Rename(tempPath, path)
	}
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			lastErr = err
		} else if err := os.Rename(tempPath, path); err != nil {
			lastErr = err
		} else {
			return nil
		}
		if time.Now().After(deadline) {
			return lastErr
		}
		time.Sleep(10 * time.Millisecond)
	}
}
