package store

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// migrateEmbeddedSnapshots 把「快照夹在文件中间」的旧日志滚动成「归档 + base」。
//
// 背景：M6 到 8a5b8ce 之间，压缩快照被当成普通事件追加进当前文件，
// journal.Compact 从未被调用——所以文件一直在变大，压缩之前的历史也没有归档可翻。
// 新代码已经修好，但**用户已有的文件**里还是夹着快照，需要一次性迁移：
//
//  1. 先完整备份为 session.pre-migrate-<ts>.jsonl（PruneEmpty 见到别的文件就不删）；
//  2. 每个快照作为切点：快照之前的消息写成 archive-NNNN.jsonl（按时间从早到晚编号）；
//  3. base 改写成「最后一个快照 + 它之后的事件」，补 archived_as / kept_from_seq；
//  4. 全程先写 tmp 再 rename；任一步失败就删掉新建的归档并保留原文件。
//
// 幂等：只在「第 0 行不是快照、且中间有快照」时才动手；迁移过的文件第一行就是快照。
func (s *JsonlStore) migrateEmbeddedSnapshots() error {
	result, err := readEvents(s.path(), false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return nil // 读不了就当没有，repair 那边会处理
	}
	if len(result.Warnings) > 0 {
		// 这次打开刚修过损坏（备份成 session.corrupt-*）。先让用户看到警告，
		// 修复后的文件下次打开再迁移，避免"迁移覆盖掉刚修好的内容"。
		return nil
	}
	events := result.Events
	if len(events) == 0 {
		return nil
	}
	// 找快照位置；第 0 行是快照说明已经是滚动过的格式
	var cuts []int
	for index, event := range events {
		if event.Type == TypeCompactSnapshot {
			cuts = append(cuts, index)
		}
	}
	if len(cuts) == 0 || cuts[0] == 0 {
		return nil
	}

	// 上次迁移在"归档写完、base 还没换"时崩溃会留下多余归档；
	// 它们的序号必然大于快照里记录的 archived_as。先清掉，否则会重复。
	recorded := uint32(0)
	for _, event := range events {
		if event.Type == TypeCompactSnapshot {
			if seq := archivedAs(event.Data); seq > recorded {
				recorded = seq
			}
		}
	}
	if names, namesErr := s.archiveNames(); namesErr == nil {
		for _, name := range names {
			if seq, ok := archiveSeqOf(name); ok && seq > recorded {
				_ = os.Remove(filepath.Join(s.dir, name))
			}
		}
	}
	if err := s.scanArchiveSeq(); err != nil {
		return err
	}

	if err := s.backupBeforeMigrate(); err != nil {
		return err
	}

	// 切段：第 k 段以第 k-1 个快照开头、到第 k 个快照之前（与 Compact 的归档一致，
	// 这样归档里也带着"那次压缩的摘要"）。
	var created []string
	previous := 0
	archiveSeq := s.archive
	for index, cut := range cuts {
		start := previous
		if index > 0 {
			start = cuts[index-1]
		}
		segment := events[start:cut]
		if len(segment) == 0 {
			previous = cut + 1
			continue
		}
		archiveSeq++
		name := s.archiveNameFor(archiveSeq)
		if err := writeArchive(filepath.Join(s.dir, name), segment); err != nil {
			cleanupArchives(s.dir, created)
			return fmt.Errorf("写归档失败：%w", err)
		}
		created = append(created, name)
		previous = cut + 1
	}

	last := events[cuts[len(cuts)-1]]
	base := events[cuts[len(cuts)-1]:]
	lastSeq := events[len(events)-1].Seq
	last.Data = withArchivedAs(last.Data, archiveSeq)
	last.Data = withKeptFromSeq(last.Data, lastSeq)
	base[0] = last
	if err := writeBaseAtomic(s.dir, base); err != nil {
		cleanupArchives(s.dir, created)
		return fmt.Errorf("重写会话日志失败：%w", err)
	}
	if err := syncDir(s.dir); err != nil {
		return err
	}
	s.archive = archiveSeq
	return nil
}

// writeArchive 把一段事件写成归档文件（先 tmp 再 rename）。
func writeArchive(path string, events []Event) error {
	tmp := path + ".tmp"
	file, err := os.Create(tmp)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			_ = file.Close()
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := renameWithRetry(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// writeBaseAtomic 用给定事件重写 session.jsonl（tmp + rename）。
func writeBaseAtomic(dir string, events []Event) error {
	tmp := filepath.Join(dir, tmpName)
	file, err := os.Create(tmp)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			_ = file.Close()
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := renameWithRetry(tmp, filepath.Join(dir, currentName)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func cleanupArchives(dir string, names []string) {
	for _, name := range names {
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// AppendEvent 把一条事件按 JSONL 写进文件（测试与迁移工具用）。
func AppendEvent(file *os.File, event Event) error {
	line, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = file.Write(append(line, '\n'))
	return err
}

// FirstEvent 读回文件的第一条事件（测试与诊断用，不依赖 *testing.T）。
func FirstEvent(path string) (Event, error) {
	result, err := readEvents(path, false)
	if err != nil {
		return Event{}, err
	}
	if len(result.Events) == 0 {
		return Event{}, fmt.Errorf("%s 里没有事件", path)
	}
	return result.Events[0], nil
}

// ReadEvents 读回文件里的全部事件（测试与诊断用）。
func ReadEvents(path string) ([]Event, error) {
	result, err := readEvents(path, false)
	if err != nil {
		return nil, err
	}
	return result.Events, nil
}

// Renumber 按顺序重排 seq（测试用：还原旧格式"一条条追加"的连续序号）。
func Renumber(events []Event) []Event {
	out := make([]Event, len(events))
	copy(out, events)
	for index := range out {
		out[index].Seq = uint64(index)
	}
	return out
}

// WriteEvents 把事件写成 JSONL（测试用）。
func WriteEvents(path string, events []Event) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	for _, event := range events {
		if err := AppendEvent(file, event); err != nil {
			return err
		}
	}
	return nil
}

// backupBeforeMigrate 迁移前整份备份；内容相同的备份已存在就不再生成，
// 避免某个文件反复迁移失败时备份越堆越多。
func (s *JsonlStore) backupBeforeMigrate() error {
	data, err := os.ReadFile(s.path())
	if err != nil {
		return fmt.Errorf("读取会话日志失败：%w", err)
	}
	sum := sha256.Sum256(data)
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("读取会话目录失败：%w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "session.pre-migrate-") {
			continue
		}
		existing, readErr := os.ReadFile(filepath.Join(s.dir, name))
		if readErr != nil || len(existing) != len(data) {
			continue
		}
		if sha256.Sum256(existing) == sum {
			return nil
		}
	}
	backup := filepath.Join(s.dir, fmt.Sprintf("session.pre-migrate-%d.jsonl", time.Now().UnixNano()))
	if err := os.WriteFile(backup, data, 0o644); err != nil {
		return fmt.Errorf("备份迁移前的会话日志失败：%w", err)
	}
	if err := syncFile(backup); err != nil {
		return fmt.Errorf("同步备份文件失败：%w", err)
	}
	return nil
}
