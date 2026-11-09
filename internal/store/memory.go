package store

import (
	"fmt"
	"sync"
)

// MemoryStore 是内存后端（测试与「临时会话」用）。
//
// 与 JSONL 后端共用同一套事件与重放逻辑：compact 只做快照折叠。
type MemoryStore struct {
	mu     sync.Mutex
	events []Event
	seq    uint64
}

// NewMemory 构造内存后端。
func NewMemory() *MemoryStore { return &MemoryStore{} }

// Append 追加事件。
func (s *MemoryStore) Append(event Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	event.Version = Version
	event.Seq = s.seq
	s.events = append(s.events, event)
	s.seq++
	return nil
}

// Load 返回全部事件。
func (s *MemoryStore) Load() (LoadResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	return LoadResult{Events: out}, nil
}

// Compact 折叠成快照（只保留快照 + 之后的事件）。
func (s *MemoryStore) Compact(snapshot Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) > 0 && s.events[len(s.events)-1].Type == TypeCompactSnapshot {
		return fmt.Errorf("连续压缩两次没有意义")
	}
	snapshot.Version = Version
	snapshot.Seq = 0
	s.events = []Event{snapshot}
	s.seq = 1
	return nil
}

// LoadOlder 内存后端没有更早的历史。
func (s *MemoryStore) LoadOlder(*ArchiveCursor) (OlderPage, error) {
	return OlderPage{}, nil
}

// Sync 内存后端无需同步。
func (s *MemoryStore) Sync() error { return nil }

// Close 内存后端无需关闭。
func (s *MemoryStore) Close() error { return nil }

var _ SessionStore = (*MemoryStore)(nil)
