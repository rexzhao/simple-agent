//go:build windows

package sessions

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSaveMetadataRetriesAtomicReplaceWhileDestinationIsLocked(t *testing.T) {
	store := NewV2Store(t.TempDir())
	saved, err := store.SaveMetadata(SessionV2{ID: "session-1", DisplayName: "before"})
	if err != nil {
		t.Fatalf("SaveMetadata(initial) error = %v", err)
	}

	metadataPath := filepath.Join(store.root, saved.ID, "meta.json")
	path, err := syscall.UTF16PtrFromString(metadataPath)
	if err != nil {
		t.Fatalf("UTF16PtrFromString(%q) error = %v", metadataPath, err)
	}
	handle, err := syscall.CreateFile(
		path,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatalf("CreateFile(%q) error = %v", metadataPath, err)
	}

	released := make(chan error, 1)
	go func() {
		time.Sleep(75 * time.Millisecond)
		released <- syscall.CloseHandle(handle)
	}()

	saved.DisplayName = "after"
	updated, saveErr := store.SaveMetadata(saved)
	closeErr := <-released
	if closeErr != nil {
		t.Fatalf("CloseHandle(%q) error = %v", metadataPath, closeErr)
	}
	if saveErr != nil {
		t.Fatalf("SaveMetadata(locked destination) error = %v", saveErr)
	}
	if updated.DisplayName != "after" {
		t.Fatalf("updated display name = %q, want after", updated.DisplayName)
	}

	loaded, err := store.Load(saved.ID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.DisplayName != "after" {
		t.Fatalf("persisted display name = %q, want after", loaded.DisplayName)
	}
}
