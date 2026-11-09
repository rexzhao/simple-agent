package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	json "encoding/json/v2"
)

const (
	currentName = "session.jsonl"
	tmpName     = "session.jsonl.tmp"
	archiveGlob = "archive-*.jsonl"
)

// JsonlStore 是 JSONL 文件后端。
type JsonlStore struct {
	dir     string
	file    *os.File
	nextSeq uint64
	archive uint32
	// warnings 记录打开时修文件产生的问题，随 Load 一起交给调用方。
	warnings []string
}

// Open 打开（或创建）目录里的会话日志。
//
// 启动时会清理残留的 `*.tmp`（上次滚动被中断留下的）。
func Open(dir string) (*JsonlStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建会话目录失败：%w", err)
	}
	store := &JsonlStore{dir: dir}
	if err := store.cleanTmp(); err != nil {
		return nil, err
	}
	// 先修复（截半行/留档中间损坏并产生 Warnings），再迁移旧格式。
	// 顺序反了会绕过损坏处理：迁移读到的是被截断的合法前缀，损坏点之后的事件
	// 就从 base 里静默消失（只剩在备份里），用户还看不到警告。
	if err := store.openCurrent(); err != nil {
		return nil, err
	}
	// 迁移会用 rename 换掉文件：句柄必须先关（Windows 上打开着的文件改不了名）
	if store.file != nil {
		_ = store.file.Close()
		store.file = nil
	}
	if err := store.scanArchiveSeq(); err != nil {
		return nil, err
	}
	if err := store.migrateEmbeddedSnapshots(); err != nil {
		return nil, err
	}
	if err := store.openCurrent(); err != nil {
		return nil, err
	}
	if err := store.dropOrphanArchives(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *JsonlStore) path() string { return filepath.Join(s.dir, currentName) }

// cleanTmp 删除残留的临时文件。
func (s *JsonlStore) cleanTmp() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("读取会话目录失败：%w", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			_ = os.Remove(filepath.Join(s.dir, entry.Name()))
		}
	}
	return nil
}

// openCurrent 打开当前文件并定位下一个 seq。
//
// 打开之前先修一遍文件（D1）：
//   - 末尾没有换行 → 说明上次写到一半就崩了，截掉最后一行残余。
//     不修的话，之后追加的每条事件都会接在半行后面，每次加载都被当成
//     「中间坏行」截断——崩溃之后写的东西会**永久丢失**（探针实测过）。
//   - 中间行损坏 → 不能继续往这个文件追加（同样会被反复截断）。把原文件
//     留档为 session.corrupt-<时间戳>.jsonl，再用已解析出的合法前缀重写。
func (s *JsonlStore) openCurrent() error {
	if err := s.repairFile(); err != nil {
		return err
	}
	file, err := os.OpenFile(s.path(), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("打开会话日志失败：%w", err)
	}
	s.file = file

	// 修完之后重新解析一次，定位下一个 seq
	result, err := readEvents(s.path(), false)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("读取会话日志失败：%w", err)
	}
	if len(result.Events) > 0 {
		last := result.Events[len(result.Events)-1]
		s.nextSeq = last.Seq + 1
	}
	return nil
}

// repairFile 修掉半行与中间损坏（见 openCurrent 的说明）。
func (s *JsonlStore) repairFile() error {
	path := s.path()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取会话日志失败：%w", err)
	}
	if len(data) == 0 {
		return nil
	}

	// 1. 末尾半行：截到最后一个换行之后
	if data[len(data)-1] != '\n' {
		if index := lastNewline(data); index >= 0 {
			data = data[:index+1]
		} else {
			// 整个文件就一行残片：清空
			data = nil
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("修复会话日志失败：%w", err)
		}
		s.warnings = append(s.warnings, "上次写入被中断，已截掉末尾的半行")
	}

	// 2. 中间行损坏：留档原文件，用合法前缀重写
	result := scanEvents(data)
	if result.brokenAt >= 0 {
		backup := filepath.Join(s.dir,
			fmt.Sprintf("session.corrupt-%d.jsonl", time.Now().UnixNano()))
		if err := os.Rename(path, backup); err != nil {
			return fmt.Errorf("留档损坏的会话日志失败：%w", err)
		}
		var builder []byte
		for _, event := range result.events {
			line, marshalErr := json.Marshal(event)
			if marshalErr != nil {
				continue
			}
			builder = append(builder, line...)
			builder = append(builder, '\n')
		}
		if err := os.WriteFile(path, builder, 0o644); err != nil {
			return fmt.Errorf("重写会话日志失败：%w", err)
		}
		s.warnings = append(s.warnings, fmt.Sprintf(
			"会话日志第 %d 行损坏：原文件已留档为 %s，保留前 %d 条事件",
			result.brokenAt+1, filepath.Base(backup), len(result.events)))
	}
	return nil
}

// lastNewline 返回最后一个 \n 的下标（没有则 -1）。
func lastNewline(data []byte) int {
	for i := len(data) - 1; i >= 0; i-- {
		if data[i] == '\n' {
			return i
		}
	}
	return -1
}

// withArchivedAs 往快照的 data 里补 archived_as 字段。
func withArchivedAs(data []byte, seq uint32) []byte {
	payload := map[string]any{}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &payload)
	}
	payload["archived_as"] = seq
	encoded, err := json.Marshal(payload)
	if err != nil {
		return data
	}
	return encoded
}

// withKeptFromSeq 往快照的 data 里补 kept_from_seq。
func withKeptFromSeq(data []byte, seq uint64) []byte {
	payload := map[string]any{}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &payload)
	}
	payload["kept_from_seq"] = seq
	encoded, err := json.Marshal(payload)
	if err != nil {
		return data
	}
	return encoded
}

// KeptFromSeq 读出快照里的 kept_from_seq（没有则 0）。
func KeptFromSeq(data []byte) uint64 {
	var payload struct {
		KeptFromSeq uint64 `json:"kept_from_seq"`
	}
	if len(data) == 0 {
		return 0
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return 0
	}
	return payload.KeptFromSeq
}

// archivedAs 读出快照里的 archived_as（没有则 0）。
func archivedAs(data []byte) uint32 {
	var payload struct {
		ArchivedAs uint32 `json:"archived_as"`
	}
	if len(data) == 0 {
		return 0
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return 0
	}
	return payload.ArchivedAs
}

// scanArchiveSeq 找到现有归档的最大序号。
func (s *JsonlStore) scanArchiveSeq() error {
	names, err := s.archiveNames()
	if err != nil {
		return err
	}
	for _, name := range names {
		if seq, ok := archiveSeqOf(name); ok && seq > s.archive {
			s.archive = seq
		}
	}
	return nil
}

// dropOrphanArchives 删掉「被中断的压缩」留下的归档。
//
// 崩溃点：复制归档之后、rename 新 base 之前。归档已存在，base 还是旧的，
// 重启后再压缩会写下一号，于是旧归档成了一份过期副本——向上滚动时同一批
// 消息会出现两次。判据：base 快照里记录的 archived_as 之后不该有归档。
func (s *JsonlStore) dropOrphanArchives() error {
	recorded := uint32(0)
	if result, err := readEvents(s.path(), false); err == nil && len(result.Events) > 0 {
		recorded = archivedAs(result.Events[0].Data)
	}
	names, err := s.archiveNames()
	if err != nil {
		return err
	}
	for _, name := range names {
		seq, ok := archiveSeqOf(name)
		if !ok || seq <= recorded {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, name)); err != nil {
			return fmt.Errorf("清理孤儿归档 %s 失败：%w", name, err)
		}
	}
	// 清理之后重算最大序号（下次压缩会重用这个号）
	s.archive = recorded
	for _, name := range names {
		if seq, ok := archiveSeqOf(name); ok && seq <= recorded && seq > s.archive {
			s.archive = seq
		}
	}
	return nil
}

// archiveNames 返回按文件名排序的归档列表。
func (s *JsonlStore) archiveNames() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("读取会话目录失败：%w", err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, "archive-") && strings.HasSuffix(name, ".jsonl") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// archiveSeqOf 解析 archive-0001.jsonl 的序号。
func archiveSeqOf(name string) (uint32, bool) {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(name, "archive-"), ".jsonl")
	value, err := strconv.ParseUint(trimmed, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(value), true
}

// Append 追加一个事件。
//
// 不用 bufio：进程崩溃时内核仍会写盘，但用户态缓冲会丢——那正是我们要避免的。
func (s *JsonlStore) Append(event Event) error {
	if s.file == nil {
		return fmt.Errorf("会话日志已关闭")
	}
	event.Version = Version
	event.Seq = s.nextSeq
	event.At = event.At.UTC()

	line, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("编码事件失败：%w", err)
	}
	line = append(line, '\n')
	if _, err := s.file.Write(line); err != nil {
		return fmt.Errorf("写入会话日志失败：%w", err)
	}
	s.nextSeq++
	return nil
}

// Load 读取最新日志。
func (s *JsonlStore) Load() (LoadResult, error) {
	result, err := readEvents(s.path(), true)
	if errors.Is(err, os.ErrNotExist) {
		return LoadResult{Warnings: s.warnings}, nil
	}
	if err != nil {
		return LoadResult{}, err
	}
	// 打开时修文件产生的问题也要交给调用方（界面提示用户）
	result.Warnings = append(s.warnings, result.Warnings...)
	return result, nil
}

// Sync 把数据刷到磁盘。
func (s *JsonlStore) Sync() error {
	if s.file == nil {
		return nil
	}
	return s.file.Sync()
}

// Close 关闭文件。
func (s *JsonlStore) Close() error {
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

// Compact 滚动日志：归档当前内容，再用快照开一份新文件。
//
// 顺序（每步都保证「不带后缀的文件始终完整」）：
//  1. 归档不存在时，把当前文件**复制**过去（不是 rename：rename 会有一段
//     没有 session.jsonl 的窗口，崩溃后会话就丢了）；
//  2. 写 tmp（快照一行）；
//  3. rename(tmp, session.jsonl) —— 同目录 rename 在 Windows/POSIX 都原子。
func (s *JsonlStore) Compact(snapshot Event) error {
	if s.file != nil {
		if err := s.file.Sync(); err != nil {
			return err
		}
		if err := s.file.Close(); err != nil {
			return err
		}
		s.file = nil
	}

	archiveSeq := s.archive + 1
	archiveName := s.archiveNameFor(archiveSeq)
	archivePath := filepath.Join(s.dir, archiveName)
	if _, err := os.Stat(archivePath); errors.Is(err, os.ErrNotExist) {
		if err := copyFile(s.path(), archivePath); err != nil {
			return fmt.Errorf("归档会话日志失败：%w", err)
		}
		if err := syncFile(archivePath); err != nil {
			return err
		}
		s.archive++
	}

	snapshot.Version = Version
	snapshot.Seq = 0
	snapshot.At = snapshot.At.UTC()
	// 记录「这份 base 对应哪一号归档」：崩溃在复制归档之后、rename 之前时，
	// 归档已经存在而 base 还是旧的。重启后这个字段能识别出孤儿归档。
	snapshot.Data = withArchivedAs(snapshot.Data, archiveSeq)
	// kept_from_seq：base 里最后一条事件的序号。按需加载归档时用它截断，
	// 避免和 kept 部分重复（D2）。
	snapshot.Data = withKeptFromSeq(snapshot.Data, s.nextSeq-1)
	line, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("编码快照失败：%w", err)
	}
	tmpPath := filepath.Join(s.dir, tmpName)
	if err := os.WriteFile(tmpPath, append(line, '\n'), 0o644); err != nil {
		return fmt.Errorf("写入快照失败：%w", err)
	}
	if err := syncFile(tmpPath); err != nil {
		return err
	}
	if err := renameWithRetry(tmpPath, s.path()); err != nil {
		return fmt.Errorf("替换会话日志失败：%w", err)
	}
	if err := syncDir(s.dir); err != nil {
		return err
	}
	s.nextSeq = 1 // seq=0 是快照本身
	return s.openCurrent()
}

func (s *JsonlStore) archiveNameFor(seq uint32) string {
	return fmt.Sprintf("archive-%04d.jsonl", seq)
}

// LoadOlder 读取一份更早的归档。
//
// 一次返回**整份**归档（一个压缩周期的事件），cursor 为 nil 时从最近的开始。
func (s *JsonlStore) LoadOlder(cursor *ArchiveCursor) (OlderPage, error) {
	names, err := s.archiveNames()
	if err != nil {
		return OlderPage{}, err
	}
	if len(names) == 0 {
		return OlderPage{}, nil
	}

	// 归档按文件名升序，越靠后越新。cursor 表示「这一页读哪个文件」，
	// nil 表示从最近的一份开始。
	index := len(names) - 1
	if cursor != nil {
		index = -1
		for i, name := range names {
			if name == cursor.Name {
				index = i
				break
			}
		}
		if index < 0 {
			return OlderPage{}, nil
		}
	}

	name := names[index]
	result, err := readEvents(filepath.Join(s.dir, name), false)
	if err != nil {
		return OlderPage{}, err
	}
	page := OlderPage{Events: result.Events, Warnings: result.Warnings}
	if index > 0 {
		// 下一次读它前面那份
		page.Next = &ArchiveCursor{Name: names[index-1]}
	}
	return page, nil
}

// scanEvents 解析已读入内存的数据，返回合法前缀与第一个坏行的下标。
type scanResult struct {
	events   []Event
	brokenAt int
}

func scanEvents(data []byte) scanResult {
	result := scanResult{brokenAt: -1}
	lines := splitJSONLines(data)
	for index, line := range lines {
		if len(line) == 0 {
			continue
		}
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			result.brokenAt = index
			return result
		}
		if len(result.events) > 0 {
			previous := result.events[len(result.events)-1]
			if event.Seq != previous.Seq+1 {
				result.brokenAt = index
				return result
			}
		}
		result.events = append(result.events, event)
	}
	return result
}

// readEvents 解析 JSONL 文件。
//
// 容错规则（设计文档 §6.1）：
//   - 最后一行解析失败 → 丢弃它（半行，上次写入被中断）；
//   - 中间某行失败或 seq 不连续 → 记警告并在此截断（之后的数据不可信）。
func readEvents(path string, reportTruncation bool) (LoadResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return LoadResult{}, err
	}

	var result LoadResult
	lines := splitJSONLines(data)
	for index, line := range lines {
		if len(line) == 0 {
			continue
		}
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			if index == len(lines)-1 {
				// 末尾半行：正常情况（写入被中断），丢弃即可
				if reportTruncation {
					result.Truncated = true
				}
				continue
			}
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("第 %d 行损坏，已在此截断：%v", index+1, err))
			break
		}
		if len(result.Events) > 0 {
			previous := result.Events[len(result.Events)-1]
			if event.Seq != previous.Seq+1 {
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("第 %d 行序号跳变（%d → %d），已在此截断", index+1, previous.Seq, event.Seq))
				break
			}
		}
		result.Events = append(result.Events, event)
	}
	return result, nil
}

// splitJSONLines 按行切分（去掉行尾 \r，兼容 Windows 换行）。
func splitJSONLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			line := data[start:i]
			if n := len(line); n > 0 && line[n-1] == '\r' {
				line = line[:n-1]
			}
			lines = append(lines, line)
			start = i + 1
		}
	}
	if start < len(data) {
		line := data[start:]
		if n := len(line); n > 0 && line[n-1] == '\r' {
			line = line[:n-1]
		}
		lines = append(lines, line)
	}
	return lines
}

// copyFile 复制文件（归档用）。
func copyFile(from, to string) error {
	data, err := os.ReadFile(from)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// 当前文件还不存在：归档一个空文件，保证步骤 1 幂等
			return os.WriteFile(to, nil, 0o644)
		}
		return err
	}
	return os.WriteFile(to, data, 0o644)
}

// syncFile 打开并 fsync 一个文件。
func syncFile(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

var _ SessionStore = (*JsonlStore)(nil)
