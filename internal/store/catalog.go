package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Catalog 管理数据目录下的全部会话。
//
// 目录布局：`<data_dir>/sessions/<context_id>/session.jsonl`。启动时扫描一遍，
// 只读每个文件的第一条（标题）与最后一条（更新时间），不重放全文——重放交给
// driver，列表只需要够显示的信息。
type Catalog struct {
	dataDir string
}

// NewCatalog 构造目录索引。
func NewCatalog(dataDir string) *Catalog { return &Catalog{dataDir: dataDir} }

// SessionsDir 返回会话根目录。
func (c *Catalog) SessionsDir() string { return filepath.Join(c.dataDir, "sessions") }

// Summary 是会话列表里的一项。
type Summary struct {
	ContextID string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
	// Workspace 是会话的工作区（旧日志为空，界面归到「默认」项目）。
	Workspace string
}

// List 返回全部会话（按更新时间倒序，最新的在前）。
func (c *Catalog) List() ([]Summary, error) {
	root := c.SessionsDir()
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取会话目录失败：%w", err)
	}

	var out []Summary
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		summary := Summary{ContextID: id}
		result, loadErr := readEvents(filepath.Join(root, id, currentName), false)
		if loadErr == nil && len(result.Events) > 0 {
			if created := firstSessionCreated(result.Events); created != nil {
				summary.Title = created.Title
				summary.CreatedAt = created.CreatedAt
			}
			summary.UpdatedAt = result.Events[len(result.Events)-1].At
			// 重命名是后续事件，可能比 session.created 的标题新
			if title := lastTitle(result.Events); title != "" {
				summary.Title = title
			}
		}
		out = append(out, summary)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].ContextID > out[j].ContextID
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

// hasOtherFiles 报告会话目录里除了主日志之外还有没有别的文件。
func hasOtherFiles(dir string) bool {
	items, err := os.ReadDir(dir)
	if err != nil {
		return true // 读不了就当"有"，宁可留着
	}
	for _, item := range items {
		if item.IsDir() {
			return true
		}
		if item.Name() != currentName {
			return true
		}
	}
	return false
}

// PruneEmpty 删除「没有任何内容」的会话目录，返回被删掉的 id。
//
// 判定**保守**：日志里一条 message.appended / message.finalized / tool.results /
// ask.* / notice.appended 都没有，并且没有归档文件——也就是用户从没发过消息。
// 以前每次启动都会建一个这样的空会话，侧栏就会堆满「新会话」。
func (c *Catalog) PruneEmpty() ([]string, error) {
	root := c.SessionsDir()
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取会话目录失败：%w", err)
	}

	var removed []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		dir := filepath.Join(root, id)
		result, readErr := readEvents(filepath.Join(dir, currentName), false)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			continue // 读不了就不动它
		}
		if hasContent(result.Events) {
			continue
		}
		// 除了 session.jsonl 之外只要还有别的文件就**不要删**：那里可能有
		// 归档（说明曾压缩过）或 session.corrupt-*.jsonl（损坏日志的备份）。
		// 后者的会话目录往往正是 session.jsonl 缺失/为空的那种，误删就把备份毁了。
		if hasOtherFiles(dir) {
			continue
		}
		if err := os.RemoveAll(dir); err == nil {
			removed = append(removed, id)
		}
	}
	return removed, nil
}

// hasContent 判断事件流里是否有任何真实内容（消息、工具、提问、通知）。
func hasContent(events []Event) bool {
	for _, event := range events {
		switch event.Type {
		case TypeMessageAppended, TypeMessageFinalize, TypeToolResults,
			TypeAskRaised, TypeAskAnswered, TypeNotice:
			return true
		}
	}
	return false
}

// Open 打开指定会话的日志。
func (c *Catalog) Open(contextID string) (SessionStore, error) {
	if contextID == "" {
		return nil, fmt.Errorf("会话 id 为空")
	}
	return Open(filepath.Join(c.SessionsDir(), contextID))
}

// Delete 删除一个会话的整个目录（日志与归档都删掉）。
//
// 调用方必须先关闭该会话的日志：Windows 上打开着的文件删不掉。
func (c *Catalog) Delete(contextID string) error {
	if contextID == "" {
		return fmt.Errorf("会话 id 为空")
	}
	dir := filepath.Join(c.SessionsDir(), contextID)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("删除会话目录失败：%w", err)
	}
	return nil
}

// Create 生成一个新会话 id 并打开它的日志。
func (c *Catalog) Create(contextID string) (SessionStore, error) {
	return c.Open(contextID)
}

// firstSessionCreated 取第一条 session.created。
func firstSessionCreated(events []Event) *SessionCreatedData {
	for _, event := range events {
		if event.Type != TypeSessionCreated {
			continue
		}
		var data SessionCreatedData
		if err := event.Decode(&data); err != nil {
			return nil
		}
		return &data
	}
	return nil
}

// lastTitle 取最后一条 session.renamed 的标题。
func lastTitle(events []Event) string {
	title := ""
	for _, event := range events {
		if event.Type != TypeSessionRenamed {
			continue
		}
		var data SessionRenamedData
		if err := event.Decode(&data); err == nil {
			title = data.Title
		}
	}
	return title
}
