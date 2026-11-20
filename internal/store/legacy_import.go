package store

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// 老数据登记：把 `<data>/sessions/<id>/` 的会话补进项目档案。
//
// 背景：改成"启动只读项目档案"之后，没登记的会话在侧栏里**看不见**
// （文件都还在，只是没人认识它们）。对已经用了一阵的用户，这等于数据丢了。
//
// **不移动任何文件**：档案给每个会话记一个相对路径（SessionRecord.Dir），
// 老会话继续留在 `sessions/<id>/`，新会话落在
// `projects/<项目id>/sessions/<id>/`，两种位置档案都能表达。
// 这样升级不碰用户数据，也不会在 Windows 上撞文件锁。
//
// 幂等：已登记的跳过。

// LegacySessionsDir 是老布局的会话根目录。
func (c *Catalog) LegacySessionsDir() string { return filepath.Join(c.dataDir, "sessions") }

// MigrateLegacySessions 把老布局里还没登记的会话补进项目档案。
//
// 返回登记了几个。
func (c *Catalog) MigrateLegacySessions() (int, error) {
	root := c.LegacySessionsDir()
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("读取老会话目录失败：%w", err)
	}

	known := map[string]bool{}
	if projects, listErr := c.ListProjects(); listErr == nil {
		for _, project := range projects {
			for _, record := range project.Sessions {
				known[record.ID] = true
			}
		}
	}

	migrated := 0
	for _, entry := range entries {
		if !entry.IsDir() || known[entry.Name()] {
			continue
		}
		id := entry.Name()
		absDir := filepath.Join(root, id)
		summary, ok := c.scanSessionDir(absDir)
		if !ok {
			continue // 空目录或读不了：跳过，交给 PruneEmpty
		}
		if summary.Title == "" {
			summary.Title = id
		}
		projectID, err := c.EnsureProject(summary.Workspace)
		if err != nil {
			return migrated, err
		}
		project, err := c.LoadProject(projectID)
		if err != nil {
			return migrated, err
		}
		project.Sessions = append(project.Sessions, SessionRecord{
			ID: id,
			// **记下它现在的位置**（父目录相对 project.json，这里就是
			// `../../sessions`），文件不动
			Dir: c.relativeToProject(projectID, root),
			// 登记时间：用会话自身的创建时间（老会话的真实年龄）
			CreatedAt: summary.CreatedAt.UTC().Format(time.RFC3339),
			Title:     summary.Title,
		})
		if err := c.SaveProject(project); err != nil {
			return migrated, err
		}
		migrated++
	}
	if migrated > 0 {
		log.Printf("已把 %d 个老会话登记进项目档案（文件保持原位）", migrated)
	}
	return migrated, nil
}
