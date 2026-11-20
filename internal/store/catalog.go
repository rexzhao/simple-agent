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
	// Project 是会话所属的项目名（空 = 尚未归入任何项目档案）。
	Project string
	// Compactions 是压缩次数（>0 表示历史被折叠过、有归档）。
	Compactions int
}

// List 返回**所有已登记项目**下的会话（按更新时间倒序）。
//
// 不再扫 `sessions/` 下的所有目录：那是老的"项目由会话反推"模型，
// 启动时会把用户早就不看的会话也读一遍。现在以项目档案为准 ——
// 被移除（Hidden）的项目整块跳过，连文件都不打开。
//
// 更新时间取 `session.jsonl` 的**文件修改时间**，不存进档案：
// 存档案就意味着每轮对话都要重写 project.json。
func (c *Catalog) List() ([]Summary, error) {
	projects, err := c.VisibleProjects()
	if err != nil {
		return nil, err
	}
	var out []Summary
	for _, project := range projects {
		for _, record := range project.Sessions {
			summary := Summary{
				ContextID:   record.ID,
				Title:       record.Title,
				Workspace:   project.Workspace,
				Project:     project.Name,
				Compactions: record.Compactions,
			}
			if when, parseErr := time.Parse(time.RFC3339, record.CreatedAt); parseErr == nil {
				summary.CreatedAt = when
			}
			// 最近活动时间：看文件本身。Stat 很便宜（只读 inode 元数据），
			// 而且已经在为"会话还在不在"做存在性检查了。
			dir := c.sessionDirFor(project, record)
			if info, statErr := os.Stat(filepath.Join(dir, currentName)); statErr == nil {
				summary.UpdatedAt = info.ModTime()
			} else {
				// 文件不在（被手动删了）：退回登记时间，至少还能排序
				summary.UpdatedAt = summary.CreatedAt
			}
			out = append(out, summary)
		}
	}
	// **不排序**：顺序就是档案里 `sessions` 数组的顺序（用户可调整）。
	// 界面按这个顺序原样渲染 —— 用户排好的顺序如果不被尊重，
	// "可以调整顺序"就成了摆设。
	return out, nil
}

// scanSessionDir 读一个会话目录，产出列表项（只读首末条，不重放全文）。
//
// 压缩过的会话首行是 compact.snapshot，session.created 已经被归档掉了 ——
// 这时标题和 workspace 要去归档里找（scanArchivedHeader），否则只能拿
// 会话 id 顶替，界面上就成了"一排看不出内容的乱码"。
func (c *Catalog) scanSessionDir(dir string) (Summary, bool) {
	result, loadErr := readEvents(filepath.Join(dir, currentName), false)
	if loadErr != nil || len(result.Events) == 0 {
		return Summary{}, false
	}
	summary := Summary{}
	if created := firstSessionCreated(result.Events); created != nil {
		summary.Title = created.Title
		summary.CreatedAt = created.CreatedAt
		summary.Workspace = created.Workspace
	}
	summary.UpdatedAt = result.Events[len(result.Events)-1].At
	if title := lastTitle(result.Events); title != "" {
		summary.Title = title
	}
	// 压缩过的会话：标题可能在归档里
	if summary.Title == "" || summary.Workspace == "" {
		if title, workspace := scanArchivedHeader(dir); title != "" || workspace != "" {
			if summary.Title == "" {
				summary.Title = title
			}
			if summary.Workspace == "" {
				summary.Workspace = workspace
			}
		}
	}
	return summary, true
}

// scanArchivedHeader 从最早的归档里取标题与工作区。
//
// 只读**第一份归档**：session.created 是会话的第一条事件，滚动归档时它
// 一定落在最早的归档开头。读整份归档没必要（可能几 MB）。
//
// 标题取**最后一次**改名的值：同一份归档里可能既有 created 又有 renamed。
func scanArchivedHeader(dir string) (title, workspace string) {
	names, err := filepath.Glob(filepath.Join(dir, archiveGlob))
	if err != nil || len(names) == 0 {
		return "", ""
	}
	sort.Strings(names)
	result, err := readEvents(names[0], false)
	if err != nil {
		return "", ""
	}
	for _, event := range result.Events {
		switch event.Type {
		case TypeSessionCreated:
			var data SessionCreatedData
			if event.Decode(&data) == nil {
				// 用 = 而不是仅在空时赋值：created 可能出现在 renamed 之后
				// （正常顺序不会，但归档拼接时不假设）
				title = data.Title
				workspace = data.Workspace
			}
		case TypeSessionRenamed:
			var data SessionRenamedData
			if event.Decode(&data) == nil {
				title = data.Title
			}
		}
	}
	return title, workspace
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
		case TypeMessageInput, TypeMessageOutput, TypeToolResult,
			TypeAskRaised, TypeAskAnswered, TypeNotice:
			return true
		}
	}
	return false
}

// sessionLocation 返回会话目录的绝对路径。
//
// 档案里的 `dir` 是**父目录**（相对 project.json），会话目录 = dir/ID。
// 这样一组会话能共享同一条路径：老会话都记成 `../../sessions`，
// 新会话记成 `sessions`（项目目录下）。
func (c *Catalog) sessionLocation(contextID string) (string, bool) {
	if projects, err := c.ListProjects(); err == nil {
		for _, project := range projects {
			for _, record := range project.Sessions {
				if record.ID != contextID {
					continue
				}
				return c.sessionDirFor(project, record), true
			}
		}
	}
	// 档案里没有：可能是还没登记的老会话，回退到老布局
	legacy := filepath.Join(c.SessionsDir(), contextID)
	if info, err := os.Stat(legacy); err == nil && info.IsDir() {
		return legacy, true
	}
	return "", false
}

// sessionDirFor 按档案算出会话目录的绝对路径。
func (c *Catalog) sessionDirFor(project Project, record SessionRecord) string {
	parent := c.projectSessionParentDir(project.ID, record.Dir)
	return filepath.Join(parent, record.ID)
}

// projectSessionParentDir 返回会话父目录的绝对路径。
//
// FileRecord.Dir 为空 → 默认布局：项目目录下的 `sessions/`。
// 否则相对 project.json 所在目录解析（`..` 可以跨出去，指向老的
// `sessions/` 目录）。
func (c *Catalog) projectSessionParentDir(projectID, dir string) string {
	if dir == "" {
		return filepath.Join(c.projectPath(projectID), "sessions")
	}
	fromFile := filepath.FromSlash(dir)
	if filepath.IsAbs(fromFile) {
		return fromFile // 手写的绝对路径：照用
	}
	return filepath.Clean(filepath.Join(c.projectPath(projectID), fromFile))
}

// relativeToProject 把绝对路径转成**相对 project.json 目录**的斜杠路径。
func (c *Catalog) relativeToProject(projectID, abs string) string {
	base := c.projectPath(projectID)
	rel, err := filepath.Rel(base, abs)
	if err != nil {
		// 跨盘符等情况无法相对：存绝对路径（回读时会识别）
		return filepath.ToSlash(abs)
	}
	return filepath.ToSlash(rel)
}

// projectSessionDir 返回项目下某个会话的目录（默认布局）。
func (c *Catalog) projectSessionDir(projectID, contextID string) string {
	return filepath.Join(c.projectPath(projectID), "sessions", contextID)
}

// ProjectDir 返回项目目录（供界面显示"数据在哪"）。
func (c *Catalog) ProjectDir(projectID string) string { return c.projectPath(projectID) }

// SessionDirFor 返回会话日志的目录（不要求它已存在）。
//
// 与 Open 的区别：Open 会真的建目录；这个只算路径。
// 用途：新建会话时要在"还没落盘"的情况下先把它登记进档案，
// 登记得知道它将来在哪 —— 老布局 `sessions/<id>/`。
func (c *Catalog) SessionDirFor(contextID string) (string, error) {
	if contextID == "" {
		return "", fmt.Errorf("会话 id 为空")
	}
	if dir, ok := c.sessionLocation(contextID); ok {
		return dir, nil
	}
	return filepath.Join(c.SessionsDir(), contextID), nil
}

// Open 打开指定会话的日志。
func (c *Catalog) Open(contextID string) (SessionStore, error) {
	if contextID == "" {
		return nil, fmt.Errorf("会话 id 为空")
	}
	if dir, ok := c.sessionLocation(contextID); ok {
		return Open(dir)
	}
	// 档案里没有：还没登记的新会话，先放老布局，由 AddSession 补登记。
	// 落在这里而不是项目目录，是为了**不改变已有行为**（老版本建的会话
	// 也都在这里，升级后新旧一致）。
	return Open(filepath.Join(c.SessionsDir(), contextID))
}

// Delete 删除一个会话的整个目录（日志与归档都删掉），并从项目档案里移除。
//
// 调用方必须先关闭该会话的日志：Windows 上打开着的文件删不掉。
func (c *Catalog) Delete(contextID string) error {
	if contextID == "" {
		return fmt.Errorf("会话 id 为空")
	}
	dir, ok := c.sessionLocation(contextID)
	if ok {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("删除会话目录失败：%w", err)
		}
	}
	return c.RemoveSession(contextID)
}

// Create 生成一个新会话 id 并打开它的日志。
func (c *Catalog) Create(contextID string) (SessionStore, error) {
	return c.Open(contextID)
}

// AddSession 把一个会话登记到它所属项目的档案里。
//
// workspace 为空时归到「默认」项目 —— 与界面一致（旧日志没有 workspace
// 字段，全都落在这里）。
//
// dir 是这个会话目录的绝对路径（由调用方给出，因为日志此时已经打开了）。
// 档案里存的是**父目录**（相对 project.json），因为会话目录 = 父目录/ID：
// 一组会话共享同一条路径，档案更短也更清楚。
func (c *Catalog) AddSession(contextID, title, workspace, dir string) error {
	projectID, err := c.EnsureProject(workspace)
	if err != nil {
		return err
	}
	project, err := c.LoadProject(projectID)
	if err != nil {
		return err
	}
	recordDir := ""
	if dir != "" {
		recordDir = c.relativeToProject(projectID, filepath.Dir(dir))
	}
	for i := range project.Sessions {
		if project.Sessions[i].ID != contextID {
			continue
		}
		// 已登记：补上缺失的信息。
		//
		// 新建会话时会先登记一次（那时还没有标题、也没落盘目录），
		// 首条消息后 ensurePersisted 再调一次 —— 这次要把 dir 和自动标题补上。
		changed := false
		if project.Sessions[i].Dir == "" && recordDir != "" {
			project.Sessions[i].Dir = recordDir
			changed = true
		}
		// 标题：只在调用方给了非空标题且与档案不同时更新
		// （新建时的占位标题是"新会话"，首条消息后变成首行文字）
		if title != "" && project.Sessions[i].Title != title {
			project.Sessions[i].Title = title
			changed = true
		}
		if changed {
			return c.SaveProject(project)
		}
		return nil
	}
	project.Sessions = append(project.Sessions, SessionRecord{
		ID:        contextID,
		Dir:       recordDir,
		Title:     title,
		CreatedAt: nowRFC3339(),
	})
	return c.SaveProject(project)
}

// SessionArchived 报告某个会话是否已归档（第二个返回值为 false 表示查不到）。
func (c *Catalog) SessionArchived(contextID string) (bool, bool) {
	projects, err := c.ListProjects()
	if err != nil {
		return false, false
	}
	for _, project := range projects {
		for _, record := range project.Sessions {
			if record.ID == contextID {
				return record.ArchivedAt != "", true
			}
		}
	}
	return false, false
}

// SetSessionArchived 归档/恢复一个会话（stamp 为空表示恢复）。
//
// 只改档案里的状态位，**不动日志文件**。
func (c *Catalog) SetSessionArchived(contextID, stamp string) error {
	return c.UpdateSession(contextID, func(record *SessionRecord) {
		record.ArchivedAt = stamp
	})
}

// UpdateSession 更新档案里某个会话的标题/时间/压缩次数。
//
// 找不到时静默返回：档案可能还没建（老数据），不该因为"更新列表项失败"
// 而让正常流程报错。
func (c *Catalog) UpdateSession(contextID string, apply func(*SessionRecord)) error {
	projects, err := c.ListProjects()
	if err != nil {
		return err
	}
	for _, project := range projects {
		for i := range project.Sessions {
			if project.Sessions[i].ID != contextID {
				continue
			}
			apply(&project.Sessions[i])
			return c.SaveProject(project)
		}
	}
	return nil
}

// RemoveSession 从项目档案里移除一个会话（不删文件，删除走 Delete）。
func (c *Catalog) RemoveSession(contextID string) error {
	projects, err := c.ListProjects()
	if err != nil {
		return err
	}
	for _, project := range projects {
		for i, record := range project.Sessions {
			if record.ID != contextID {
				continue
			}
			project.Sessions = append(project.Sessions[:i], project.Sessions[i+1:]...)
			return c.SaveProject(project)
		}
	}
	return nil
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
