package store_test

import (
	"testing"

	"simple-agent/internal/store"
)

// 压缩后 seq 不重置：整个会话生命周期内单调递增。
//
// 为什么重要：展示端用 seq 做缓存 key 与分页游标。若压缩后 seq 回到 1，
// 压缩那一刻所有缓存与游标都会失效——用户正在看的历史会突然错位。
func TestSeqMonotonicAcrossCompaction(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 压缩前写 3 条
	for i := 0; i < 3; i++ {
		if err := s.Append(mustEvent(t, store.TypeMessageInput, map[string]any{"i": i})); err != nil {
			t.Fatal(err)
		}
	}
	before, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	lastBefore := before.Events[len(before.Events)-1].Seq

	// 压缩
	if err := s.Compact(mustEvent(t, store.TypeCompactSnapshot, map[string]any{"summary": "x"})); err != nil {
		t.Fatal(err)
	}

	// 压缩后继续写
	if err := s.Append(mustEvent(t, store.TypeMessageInput, map[string]any{"after": 1})); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(mustEvent(t, store.TypeMessageInput, map[string]any{"after": 2})); err != nil {
		t.Fatal(err)
	}

	after, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Events) < 3 {
		t.Fatalf("压缩后应有快照 + 2 条新事件，实际 %d 条", len(after.Events))
	}

	// 快照的 seq 必须大于压缩前最后一条
	snapshotSeq := after.Events[0].Seq
	if snapshotSeq <= lastBefore {
		t.Fatalf("快照 seq（%d）应大于压缩前最后一条（%d）", snapshotSeq, lastBefore)
	}

	// 整条链严格递增
	previous := uint64(0)
	for index, event := range after.Events {
		if index > 0 && event.Seq != previous+1 {
			t.Fatalf("第 %d 条 seq=%d 不接上一条 %d（压缩后必须连续递增）",
				index, event.Seq, previous)
		}
		previous = event.Seq
	}
	if previous <= lastBefore+1 {
		t.Fatalf("最后一条 seq（%d）应远大于压缩前（%d）", previous, lastBefore)
	}
}

// 压缩后重开 store：nextSeq 要接着快照往下（不能回到 1）。
func TestReopenAfterCompactKeepsSeq(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := s.Append(mustEvent(t, store.TypeMessageInput, map[string]any{"i": i})); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Compact(mustEvent(t, store.TypeCompactSnapshot, map[string]any{"summary": "x"})); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	// 重开
	s2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	if err := s2.Append(mustEvent(t, store.TypeMessageInput, map[string]any{"after": true})); err != nil {
		t.Fatal(err)
	}
	result, err := s2.Load()
	if err != nil {
		t.Fatal(err)
	}
	last := result.Events[len(result.Events)-1]
	snapshotSeq := result.Events[0].Seq
	if last.Seq <= snapshotSeq {
		t.Fatalf("重开后新增事件的 seq（%d）应大于快照（%d）", last.Seq, snapshotSeq)
	}
	if last.Seq <= 5 {
		t.Fatalf("重开后不该从头分配 seq，实际 %d（压缩前已有 5 条）", last.Seq)
	}
}