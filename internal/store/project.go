package store

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	json "encoding/json/v2"
	"encoding/json/jsontext"
)

// 项目档案布局：
//
//	<data_dir>/projects.json              索引：项目 ID 的有序数组
//	<data_dir>/projects/<project-id>/project.json   单个项目的档案
//	<data_dir>/projects/<project-id>/sessions/<id>/ 该项目自己的会话（新会话）
//
// 为什么要有 projects.json：项目列表和**顺序**需要一个唯一权威。
// 只靠扫 `projects/` 目录的话，顺序只能靠命名或额外字段去猜；
// 而"移除项目"也没法干净表达（目录还在）。
// 现在启动只读一个 projects.json，顺序就是数组顺序，移除就是从数组里删掉。
//
// 移除**不删目录**：项目档案和会话文件原地留着，重新添加同一路径时
// 把 ID 加回数组即可恢复（用户明确要求"只隐藏，文件不动"）。
const (
	// projectsDirName 是项目目录根。
	projectsDirName = "projects"
	// projectFileName 是单个项目的档案文件名。
	projectFileName = "project.json"
	// projectsIndexName 是项目索引文件名（放在 data_dir 下）。
	projectsIndexName = "projects.json"
)

// projectsIndex 是 `projects.json` 的内容。
type projectsIndex struct {
	// Projects 是项目 ID 列表，**顺序即显示顺序**。
	Projects []string `json:"projects"`
}

// Project 是一个项目的档案。
type Project struct {
	// ID 是项目目录名（UUID，也是 `projects/<id>/` 的名字）。
	//
	// **不用项目名做目录名**：项目名来自路径末级，可能含中文、空格、
	// 大小写混用的奇怪字符，作为目录名在跨平台时容易出编码问题；
	// 重名还得加后缀。UUID 一劳永逸，可读性由 Name 字段负责。
	ID string `json:"id"`
	// Name 是项目名（显示用，来自目录末级，可重复）。
	Name string `json:"name"`
	// Workspace 是项目对应的目录（会话的工作区）。
	Workspace string `json:"workspace"`
	// Hidden 为真表示用户移除了这个项目。
	//
	// 注意：**索引里没有 = 已移除**，这是权威判据。这个字段只作为
	// 冗余标记留着（读单个 project.json 时也能看出状态），
	// 判断显示与否一律看 projects.json。
	Hidden bool `json:"hidden,omitempty"`
	// CreatedAt 是项目登记时间（RFC3339）。
	CreatedAt string `json:"created_at,omitempty"`
	// Sessions 是这个项目下的会话档案（顺序即显示顺序，用户可调整）。
	Sessions []SessionRecord `json:"sessions,omitempty"`
}

// SessionRecord 是一个会话的档案项。
//
// 只放**列表和恢复需要**的信息；会话内容仍在 session.jsonl 里，按需打开。
// 这样启动不必重放全文 —— 那是之前启动慢的根源。
type SessionRecord struct {
	// ID 是会话的 context_id。
	ID string `json:"id"`
	// Dir 是会话**所在目录**（父目录），相对 `project.json` 的路径。
	//
	// 会话目录 = `<Dir>/<ID>`。记父目录而不是会话自己的目录，是为了
	// 一组会话能共享一条路径：老会话都在 `sessions/` 下，档案里就都是
	// `../../sessions`，一眼能看出"这些还留在老位置"。
	//
	// 为空时按默认布局推（项目目录下的 `sessions/`），兼容手写的档案。
	//
	// 用相对 project.json 的路径（而不是相对 data_dir）：档案跟着项目
	// 目录一起搬动后依然有效。
	Dir string `json:"dir,omitempty"`
	// Title 是显示名（改名后由界面回写）。
	Title string `json:"title,omitempty"`
	// CreatedAt 是会话登记时间（RFC3339）。
	//
	// **不存 updated_at**：会话列表要按"最近活动"排序，而活动发生在每次
	// 对话中 —— 存进档案就意味着每轮都要重写 project.json（高频写盘）。
	// 更新时间直接取 session.jsonl 的文件修改时间，操作系统已经在维护它了。
	CreatedAt string `json:"created_at,omitempty"`
	// Compactions 是这个会话压缩过几次（>0 表示已压缩、有归档）。
	//
	// 用户明确要求"哪些会话是压缩的"要能看出来：压缩过的会话重开时要
	// 读快照而不是从头重放，用户也需要知道历史被折叠过。
	Compactions int `json:"compactions,omitempty"`
	// ArchivedAt 非空表示这个会话**已归档**（RFC3339）。
	//
	// 归档的语义是"收起来、不能再运行"：界面上仍然列出（灰显），
	// 但发消息、压缩、改名一律拒绝，必须显式恢复才能继续。
	// 与项目级的"移除"不同 —— 那个是整块不显示，也说不出它存在过。
	//
	// **不移动、不删除任何文件**：归档只是状态位，恢复就是清掉它。
	// 所以这里存的是时间而不是 bool：用户还能看出"什么时候收起来的"。
	ArchivedAt string `json:"archived_at,omitempty"`
}

// projectPath 返回某个项目的档案目录（按 **ID**，不是名字）。
func (c *Catalog) projectPath(id string) string {
	return filepath.Join(c.dataDir, projectsDirName, id)
}

// MailboxDir 返回某个项目的会话邮箱目录（`<项目>/mailbox/`）。
//
// 放在项目目录下而不是全局：邮件本来就是项目内的协作产物，
// 与会话日志同源，删项目时一并消失，也不会和别的项目串味。
func (c *Catalog) MailboxDir(projectID string) string {
	if projectID == "" {
		return ""
	}
	return filepath.Join(c.projectPath(projectID), "mailbox")
}

// ProjectOfSession 反查某个会话属于哪个项目（找不到返回空串）。
//
// 用途：新建会话时先登记再回填 ProjectID —— 登记那一步才知道它在哪个项目，
// 而会话对象在此之前就构造好了。
func (c *Catalog) ProjectOfSession(sessionID string) (string, error) {
	projects, err := c.ListProjects()
	if err != nil {
		return "", err
	}
	for _, project := range projects {
		for _, record := range project.Sessions {
			if record.ID == sessionID {
				return project.ID, nil
			}
		}
	}
	return "", nil
}

// projectFilePath 返回某个项目的档案文件。
func (c *Catalog) projectFilePath(id string) string {
	return filepath.Join(c.projectPath(id), projectFileName)
}

// projectsIndexPath 返回索引文件路径。
func (c *Catalog) projectsIndexPath() string {
	return filepath.Join(c.dataDir, projectsIndexName)
}

// loadIndex 读 `projects.json`。
//
// 文件不存在时**从目录重建**并写回：兼容"只有 projects/<id>/project.json
// 而没有索引"的情况（早期版本、或用户手工拷贝进来的项目）。
func (c *Catalog) loadIndex() ([]string, error) {
	data, err := os.ReadFile(c.projectsIndexPath())
	if errors.Is(err, os.ErrNotExist) {
		return c.rebuildIndex()
	}
	if err != nil {
		return nil, fmt.Errorf("读取 projects.json 失败：%w", err)
	}
	var index projectsIndex
	if err := json.Unmarshal(data, &index); err != nil {
		// 索引坏了不要让项目全部消失：从目录重建
		log.Printf("projects.json 解析失败（%v），正从目录重建", err)
		return c.rebuildIndex()
	}
	return index.Projects, nil
}

// saveIndex 写 `projects.json`（原子替换）。
func (c *Catalog) saveIndex(ids []string) error {
	if ids == nil {
		ids = []string{}
	}
	data, err := json.Marshal(projectsIndex{Projects: ids},
		json.Deterministic(true), jsontext.WithIndent("  "))
	if err != nil {
		return err
	}
	path := c.projectsIndexPath()
	tmp := path + ".tmp"
	if err := os.MkdirAll(c.dataDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// rebuildIndex 扫 projects/ 目录重建索引（按目录名排序，顺序稳定）。
//
// **只读不写**：写盘由调用方决定。这样 EnsureProject 能先读到"当前索引"，
// 追加自己的 ID 后一次写入 —— 否则 rebuild 会先写一遍（已含新项目），
// 调用方再追加就重复了。
func (c *Catalog) rebuildIndex() ([]string, error) {
	root := filepath.Join(c.dataDir, projectsDirName)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取项目目录失败：%w", err)
	}
	var ids []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// 只收有档案的目录：空目录不算项目
		if _, statErr := os.Stat(c.projectFilePath(entry.Name())); statErr != nil {
			continue
		}
		ids = append(ids, entry.Name())
	}
	sort.Strings(ids)
	return ids, nil
}

// ListProjects 按 `projects.json` 的顺序读出全部**可见**项目。
//
// 索引是唯一权威：不在索引里的项目一律不看（= 已移除），
// 连它的 project.json 都不读。
func (c *Catalog) ListProjects() ([]Project, error) {
	ids, err := c.loadIndex()
	if err != nil {
		return nil, err
	}
	// 索引文件不存在时顺手补建：只读路径不该反复扫目录
	if _, statErr := os.Stat(c.projectsIndexPath()); errors.Is(statErr, os.ErrNotExist) && len(ids) > 0 {
		if saveErr := c.saveIndex(ids); saveErr != nil {
			log.Printf("补建 projects.json 失败：%v", saveErr)
		}
	}
	out := make([]Project, 0, len(ids))
	for _, id := range ids {
		project, loadErr := c.LoadProject(id)
		if loadErr != nil {
			// 单个项目坏了不影响其它项目：跳过并继续
			continue
		}
		project.Hidden = false // 在索引里就是可见的
		out = append(out, project)
	}
	return out, nil
}

// LoadProject 按 ID 读一个项目的档案（不检查索引，用于恢复/调试）。
func (c *Catalog) LoadProject(id string) (Project, error) {
	data, err := os.ReadFile(c.projectFilePath(id))
	if errors.Is(err, os.ErrNotExist) {
		return Project{ID: id}, nil
	}
	if err != nil {
		return Project{}, err
	}
	var project Project
	if err := json.Unmarshal(data, &project); err != nil {
		return Project{}, fmt.Errorf("项目档案 %s 解析失败：%w", id, err)
	}
	if project.ID == "" {
		project.ID = id
	}
	return project, nil
}

// ReorderProjects 按给定顺序重写索引（界面拖完顺序后调一次）。
//
// 只写 projects.json：不动任何 project.json，所以很轻。
func (c *Catalog) ReorderProjects(ids []string) error {
	// 只保留索引里真实存在的 ID，避免误传导致项目凭空出现
	existing, err := c.loadIndex()
	if err != nil {
		return err
	}
	valid := make(map[string]bool, len(existing))
	for _, id := range existing {
		valid[id] = true
	}
	ordered := make([]string, 0, len(existing))
	seen := map[string]bool{}
	for _, id := range ids {
		if !valid[id] || seen[id] {
			continue
		}
		seen[id] = true
		ordered = append(ordered, id)
	}
	// 没提到的接在后面（传了不完整列表也不丢项目）
	for _, id := range existing {
		if !seen[id] {
			ordered = append(ordered, id)
		}
	}
	return c.saveIndex(ordered)
}

// ReorderSessions 按给定的会话 ID 顺序重写某个项目里的 Sessions 顺序。
func (c *Catalog) ReorderSessions(projectID string, ids []string) error {
	project, err := c.LoadProject(projectID)
	if err != nil {
		return err
	}
	byID := make(map[string]SessionRecord, len(project.Sessions))
	for _, record := range project.Sessions {
		byID[record.ID] = record
	}
	ordered := make([]SessionRecord, 0, len(project.Sessions))
	seen := map[string]bool{}
	for _, id := range ids {
		record, ok := byID[id]
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		ordered = append(ordered, record)
	}
	// 没提到的保留在后面（避免因为传了不完整的列表而丢会话）
	for _, record := range project.Sessions {
		if !seen[record.ID] {
			ordered = append(ordered, record)
		}
	}
	project.Sessions = ordered
	return c.SaveProject(project)
}

// VisibleProjects 只返回要显示的项目（排除被移除的）。
// VisibleProjects 只返回要显示的项目。
//
// 现在"在 projects.json 索引里"就等于"可见"，所以与 ListProjects 等价。
// 留这个别名是为了调用点读起来清楚（界面要的是"可见的"）。
func (c *Catalog) VisibleProjects() ([]Project, error) {
	return c.ListProjects()
}

// SaveProject 写项目档案（原子替换：先写 .tmp 再 rename）。
//
// 原子替换的意义：档案是项目的元信息，写坏一半会让项目读不出来。
func (c *Catalog) SaveProject(project Project) error {
	if project.ID == "" {
		return errors.New("项目 ID 不能为空")
	}
	dir := c.projectPath(project.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建项目目录失败：%w", err)
	}
	data, err := json.Marshal(project, json.Deterministic(true), jsontext.WithIndent("  "))
	if err != nil {
		return err
	}
	final := c.projectFilePath(project.ID)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// ProjectForWorkspace 按工作区路径找项目（找不到返回零值与 false）。
// ProjectForWorkspace 按工作区路径找项目（**包括已被移除的**）。
//
// 必须扫目录而不是只查索引：被移除的项目档案还在，重新添加同一路径时
// 要能找回它 —— 否则会新建一个重复项目，原来的会话就"接不上"了。
func (c *Catalog) ProjectForWorkspace(workspace string) (Project, bool) {
	want := normalizeWorkspace(workspace)
	if want == "" {
		// 空工作区（旧日志）也可能有项目，但匹配它没有意义：
		// 谁都能对上，反而会串。只在显式给路径时匹配。
		return Project{}, false
	}
	root := filepath.Join(c.dataDir, projectsDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		return Project{}, false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		project, loadErr := c.LoadProject(entry.Name())
		if loadErr != nil || project.Workspace == "" {
			continue
		}
		if normalizeWorkspace(project.Workspace) == want {
			return project, true
		}
	}
	return Project{}, false
}

// EnsureProject 确保某个工作区在档案里存在，返回项目 ID。
//
// 已存在则原样返回。**被移除的项目也算存在**：重新添加同一路径就是恢复
// —— 把它的 ID 加回索引末尾即可，会话文件一个都不用动。
func (c *Catalog) EnsureProject(workspace string) (string, error) {
	if project, ok := c.ProjectForWorkspace(workspace); ok {
		// 恢复：确保它在索引里（可能之前被移除过）
		if err := c.ensureIndexed(project.ID); err != nil {
			return "", err
		}
		return project.ID, nil
	}
	id, err := newProjectID()
	if err != nil {
		return "", err
	}
	project := Project{
		ID:        id,
		Name:      projectNameFor(workspace),
		Workspace: workspace,
		CreatedAt: nowRFC3339(),
	}
	if err := c.SaveProject(project); err != nil {
		return "", err
	}
	// 新项目排在索引末尾。
	//
	// 注意：这里 loadIndex 读到的可能**已经包含**新项目 —— 索引文件不存在
	// 时它会扫目录重建，而 SaveProject 刚把目录建出来了。所以不能只判断
	// "在不在"，必须保证**落盘一次**：索引文件在 rebuild 阶段是只读的，
	// 不写就永远没有 projects.json。
	ids, err := c.loadIndex()
	if err != nil {
		return "", err
	}
	for _, existing := range ids {
		if existing == id {
			return id, c.saveIndex(ids) // 已在列表里，但要确保索引文件存在
		}
	}
	return id, c.saveIndex(append(ids, id))
}

// ensureIndexed 保证某个项目 ID 在索引里（不在就追加到末尾）。
//
// 无论在不在都写一次：索引文件可能还没落盘（rebuild 是只读的）。
func (c *Catalog) ensureIndexed(id string) error {
	ids, err := c.loadIndex()
	if err != nil {
		return err
	}
	for _, existing := range ids {
		if existing == id {
			return c.saveIndex(ids)
		}
	}
	return c.saveIndex(append(ids, id))
}

// RemoveProject 移除一个项目：**从 projects.json 索引里删掉**。
//
// 只动索引，**不删任何文件**：项目档案和会话都原地留着，
// 重新添加同一工作区时 EnsureProject 会把 ID 加回索引，一切照旧。
// 这就是用户要的"移除项目不显示，添加回来就恢复"。
func (c *Catalog) RemoveProject(id string) error {
	ids, err := c.loadIndex()
	if err != nil {
		return err
	}
	out := make([]string, 0, len(ids))
	for _, existing := range ids {
		if existing != id {
			out = append(out, existing)
		}
	}
	return c.saveIndex(out)
}

// SetProjectHidden 兼容旧接口：true = 从索引移除，false = 加回索引末尾。
//
// 早期版本用 project.json 里的 hidden 字段表达"移除"；改成索引之后
// 语义等价，保留这个方法让调用点不用改。
func (c *Catalog) SetProjectHidden(id string, hidden bool) error {
	if hidden {
		return c.RemoveProject(id)
	}
	return c.ensureIndexed(id)
}

// newProjectID 生成项目目录名（UUID v4 的十六进制形式，32 个字符）。
//
// 不用项目名做目录名：它来自路径末级，可能含中文/空格/怪字符，
// 跨平台容易出编码问题，重名还得加后缀。
func newProjectID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("生成项目 ID 失败：%w", err)
	}
	// RFC 4122 v4
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16]), nil
}

// projectNameFor 从工作区路径取一个可读的项目名（末级目录名）。
//
// 这个名字只用于**显示和排序**，不参与任何路径拼接 —— 所以中文、
// 空格、重名都无所谓。
func projectNameFor(workspace string) string {
	cleaned := strings.TrimRight(strings.ReplaceAll(workspace, "\\", "/"), "/")
	if idx := strings.LastIndex(cleaned, "/"); idx >= 0 && idx+1 < len(cleaned) {
		cleaned = cleaned[idx+1:]
	}
	if cleaned == "" {
		return "默认"
	}
	return cleaned
}

// nowRFC3339 返回当前 UTC 时间的 RFC3339 字符串。
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// normalizeWorkspace 做路径比较用的规范化（大小写与分隔符）。
func normalizeWorkspace(workspace string) string {
	cleaned := strings.TrimRight(strings.ReplaceAll(workspace, "\\", "/"), "/")
	// Windows 路径大小写不敏感；统一小写比较避免 "F:/Work" 与 "f:/work" 重复建项目
	return strings.ToLower(cleaned)
}