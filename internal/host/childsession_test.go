package host_test

import (
	"encoding/json"
	"testing"

	"simple-agent/internal/host"
	"simple-agent/internal/store"
)

// newChildPair 造一个父会话（带项目档案）。
func newChildPair(t *testing.T) (*host.Local, *host.Session) {
	t.Helper()
	catalog := store.NewCatalog(t.TempDir())
	l := newHostWithCatalog(t, catalog)
	return l, l.NewSession("")
}

// new_session 工具在**同一项目**里建子会话，并记录父子关系。
func TestNewChildSessionMarksParent(t *testing.T) {
	l, parent := newChildPair(t)

	childID, err := l.NewChildSession(parent, "子任务", "")
	if err != nil {
		t.Fatal(err)
	}
	child := l.Session(childID)
	if child == nil {
		t.Fatal("子会话应存在")
	}
	if child.Title != "子任务" {
		t.Fatalf("标题不对：%q", child.Title)
	}

	// 父子关系要能在会话列表里看到（侧栏靠它折叠）
	var found bool
	for _, ref := range l.Sessions() {
		if ref.ID != childID {
			continue
		}
		found = true
		if ref.ParentID != parent.ID {
			t.Fatalf("子会话的 parent 应是父会话 %s，实际 %q", parent.ID, ref.ParentID)
		}
	}
	if !found {
		t.Fatal("会话列表里应有子会话")
	}
}

// 子会话必须继承父会话的**工作区**（同一项目）。
func TestChildSessionSameProject(t *testing.T) {
	l, parent := newChildPair(t)
	childID, err := l.NewChildSession(parent, "子任务", "")
	if err != nil {
		t.Fatal(err)
	}
	child := l.Session(childID)
	if child.Workspace != parent.Workspace {
		t.Fatalf("工作区应继承父会话：%q vs %q", child.Workspace, parent.Workspace)
	}
	if child.ProjectID != parent.ProjectID {
		t.Fatalf("项目应相同：%q vs %q", child.ProjectID, parent.ProjectID)
	}
}

// 带 prompt 时子会话立刻开跑（收到那条任务）。
func TestChildSessionWithPromptRuns(t *testing.T) {
	l, parent := newChildPair(t)
	childID, err := l.NewChildSession(parent, "子任务", "帮我统计一下代码行数")
	if err != nil {
		t.Fatal(err)
	}
	child := l.Session(childID)
	if got := child.LastUserTextForTest(); got != "帮我统计一下代码行数" {
		t.Fatalf("子会话应收到初始任务，实际 %q", got)
	}
}

// 空标题要有默认名（不能是空白行）。
func TestChildSessionDefaultTitle(t *testing.T) {
	l, parent := newChildPair(t)
	childID, err := l.NewChildSession(parent, "   ", "")
	if err != nil {
		t.Fatal(err)
	}
	child := l.Session(childID)
	if child.Title == "" {
		t.Fatal("应有默认标题")
	}
}

// **子会话不能再派生**：只有顶层会话才有 new_session 能力。
//
// 用户定的规则："子会话没有创建子会话的工具，所以不应该有环"。
// 这条同时保证了侧栏的树只有一层、不会出现互相引用。
func TestChildSessionCannotSpawn(t *testing.T) {
	l, parent := newChildPair(t)
	childID, err := l.NewChildSession(parent, "子任务", "")
	if err != nil {
		t.Fatal(err)
	}
	child := l.Session(childID)
	if child == nil {
		t.Fatal("子会话应存在")
	}
	if child.ToolContext().Sessions != nil {
		t.Fatal("子会话不该有 new_session 能力")
	}
	// 父会话**有**这个能力（不然功能就没用了）
	if parent.ToolContext().Sessions == nil {
		t.Fatal("顶层会话应有 new_session 能力")
	}
}

// **子会话的工具列表里没有 new_session**（不是"调了报错"，是压根没注册）。
func TestChildSessionToolNotRegistered(t *testing.T) {
	l, parent := newChildPair(t)
	childID, err := l.NewChildSession(parent, "子任务", "")
	if err != nil {
		t.Fatal(err)
	}
	child := l.Session(childID)

	if hasTool(parent, "new_session") != true {
		t.Fatal("顶层会话应有 new_session 工具")
	}
	if hasTool(child, "new_session") {
		t.Fatal("子会话的工具列表里不该出现 new_session")
	}
}

// **父会话归档 → 子会话也算归档**（不需要子会话自己有归档标记）。
func TestChildInheritsArchivedFromParent(t *testing.T) {
	l, parent := newChildPair(t)
	childID, err := l.NewChildSession(parent, "子任务", "")
	if err != nil {
		t.Fatal(err)
	}
	child := l.Session(childID)

	if child.Archived() {
		t.Fatal("父会话没归档时子会话不该是归档")
	}
	if err := parent.SetArchived(true); err != nil {
		t.Fatal(err)
	}
	if !child.Archived() {
		t.Fatal("父会话归档后子会话应算作归档")
	}
	// 子会话**自己**的标记仍是 false（继承而非写入）
	if child.OwnArchived() {
		t.Fatal("不该给子会话写归档标记（继承即可）")
	}
	// 父会话恢复 → 子会话跟着恢复
	if err := parent.SetArchived(false); err != nil {
		t.Fatal(err)
	}
	if child.Archived() {
		t.Fatal("父会话恢复后子会话应跟着恢复")
	}
}

// 归档继承也要体现在会话列表里（侧栏靠它灰显）。
func TestArchivedInheritanceInSessionList(t *testing.T) {
	l, parent := newChildPair(t)
	childID, err := l.NewChildSession(parent, "子任务", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.SetArchived(true); err != nil {
		t.Fatal(err)
	}
	for _, ref := range l.Sessions() {
		if ref.ID != childID {
			continue
		}
		if !ref.Archived {
			t.Fatal("会话列表里子会话应显示为归档")
		}
		return
	}
	t.Fatal("列表里找不到子会话")
}

// newSession 命令带 parent_id 时走子会话路径（界面与 Android 都用这条）。
func TestNewSessionCommandWithParent(t *testing.T) {
	l, parent := newChildPair(t)

	payload, err := json.Marshal(host.NewSessionPayload{
		Title:    "子任务",
		ParentID: parent.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	cmdID, err := l.Submit(host.Command{Kind: "newSession", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if cmdID == "" {
		t.Fatal("应返回命令 id")
	}

	// 应该多出一个会话，且 parent_id 指向父会话
	var child *host.Session
	for _, ref := range l.Sessions() {
		if ref.ID == parent.ID {
			continue
		}
		child = l.Session(ref.ID)
		if ref.ParentID != parent.ID {
			t.Fatalf("parent_id 应是 %s，实际 %q", parent.ID, ref.ParentID)
		}
	}
	if child == nil {
		t.Fatalf("应创建出子会话，现有会话：%d", len(l.Sessions()))
	}
	if child.Title != "子任务" {
		t.Fatalf("标题不对：%q", child.Title)
	}
}

func hasTool(session *host.Session, name string) bool {
	for _, tool := range session.ToolsForTest() {
		if tool.Name == name {
			return true
		}
	}
	return false
}

// 父子关系要落盘：重开档案还在（否则重启后侧栏关系丢失）。
func TestChildParentPersists(t *testing.T) {
	catalog := store.NewCatalog(t.TempDir())
	l := newHostWithCatalog(t, catalog)
	parent := l.NewSession("")
	childID, err := l.NewChildSession(parent, "子任务", "")
	if err != nil {
		t.Fatal(err)
	}

	// 直接从档案读（模拟重启后的读取路径）
	projectID, err := catalog.ProjectOfSession(childID)
	if err != nil || projectID == "" {
		t.Fatalf("子会话应登记进项目档案：%v", err)
	}
	project, err := catalog.LoadProject(projectID)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range project.Sessions {
		if record.ID != childID {
			continue
		}
		if record.ParentID != parent.ID {
			t.Fatalf("档案里的 parent_id 应是 %s，实际 %q", parent.ID, record.ParentID)
		}
		return
	}
	t.Fatal("档案里找不到子会话")
}
