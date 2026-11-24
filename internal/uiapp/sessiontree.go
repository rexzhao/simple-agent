package uiapp

import "simple-agent/internal/host"

// 侧栏里的父子会话关系。
//
// 需求：由一个会话派生出的子会话（new_session 工具创建）在侧栏里**挂在
// 父会话下面**，默认不展开 —— 否则一个会话派生七八个子任务，侧栏就被
// 平铺成一长串，看不出它们属于同一件事。
//
// **只有一层**：子会话不挂 new_session 工具（见 host/childsession.go），
// 所以不存在"孙会话"。这条约束让树形逻辑简单很多，也保证了不会有环。
// 排序函数仍然写成递归形式 —— 实现代价一样，而万一将来放开层级也不用改。
//
// 折叠状态存在 App.childOpen 里（按父会话 ID），默认 false（不展开）。
// 与项目分组一样，是**界面状态**，不落盘：重启后回到默认收起。

// childSessionsOf 返回某个会话的直接子会话（保持档案里的顺序）。
func (a *App) childSessionsOf(parentID string) []*Session {
	var out []*Session
	for _, session := range a.sessions {
		if session != nil && session.parentID == parentID {
			out = append(out, session)
		}
	}
	return out
}

// childExpanded 报告某个父会话是否已展开子会话。
//
// **默认不展开**（用户明确要求）：没记录过就是收起。
func (a *App) childExpanded(parentID string) bool {
	if a.childOpen == nil {
		return false
	}
	return a.childOpen[parentID]
}

// toggleChildren 展开/收起某个父会话的子会话。
func (a *App) toggleChildren(parentID string) {
	if a.childOpen == nil {
		a.childOpen = map[string]bool{}
	}
	a.childOpen[parentID] = !a.childOpen[parentID]
}

// SubmitForTest 直接提交一条 host 命令（测试用）。
func (a *App) SubmitForTest(cmd host.Command) {
	_, _ = a.host.Submit(cmd)
}

// HostSubmitForTest 提交命令并返回错误（测试用，便于排查"命令没执行"）。
func (a *App) HostSubmitForTest(cmd host.Command) (string, error) {
	return a.host.Submit(cmd)
}

// HostSessionCountForTest 返回 host 侧的会话数（测试用）。
func (a *App) HostSessionCountForTest() int { return len(a.host.Sessions()) }

// SessionIDsForTest 返回当前会话 ID 列表（测试用）。
func (a *App) SessionIDsForTest() []string {
	out := make([]string, 0, len(a.sessions))
	for _, session := range a.sessions {
		if session != nil {
			out = append(out, session.ID)
		}
	}
	return out
}

// ChildCountForTest 返回某个会话的子会话数（测试用）。
func (a *App) ChildCountForTest(parentID string) int {
	return len(a.childSessionsOf(parentID))
}

// orderSessionsByParent 把会话按父子关系重排成展示顺序。
//
// 规则：
//   - 顶层会话按原顺序（档案顺序，用户可调整）
//   - 父会话**已展开**时，子会话紧跟其后
//   - 父会话**未展开**时，子会话不出现（默认收起）
//   - 父会话不在当前列表里（被删了、或被搜索过滤掉）的子会话按**顶层**
//     显示 —— 不能因为找不到父亲就让它消失
//
// expanded 报告某个父会话是否已展开。
// 返回顺序即侧栏里的显示顺序。
func orderSessionsByParent(sessions []*Session, expanded func(string) bool) []*Session {
	if len(sessions) == 0 {
		return nil
	}
	children := map[string][]*Session{}
	known := map[string]bool{}
	for _, session := range sessions {
		if session != nil {
			known[session.ID] = true
		}
	}
	var roots []*Session
	for _, session := range sessions {
		if session == nil {
			continue
		}
		parentID := session.parentID
		// 父会话可见 → 挂到它下面；否则当顶层
		if parentID != "" && known[parentID] && parentID != session.ID {
			children[parentID] = append(children[parentID], session)
			continue
		}
		roots = append(roots, session)
	}

	// reachable 先算出"通过父子关系能走到哪些会话"（**不考虑折叠**）。
	//
	// 用途只一个：兜底判据。走不到的才是真的丢了（脏数据），
	// 而被折叠藏起来的子会话是 reachable 的，不该被补出来。
	reachable := map[string]bool{}
	var mark func(session *Session)
	mark = func(session *Session) {
		if session == nil || reachable[session.ID] {
			return
		}
		reachable[session.ID] = true
		for _, child := range children[session.ID] {
			mark(child)
		}
	}
	for _, root := range roots {
		mark(root)
	}

	out := make([]*Session, 0, len(sessions))
	visited := map[string]bool{}
	var walk func(session *Session)
	walk = func(session *Session) {
		if session == nil || visited[session.ID] {
			return
		}
		visited[session.ID] = true
		out = append(out, session)
		// **收起时子会话不出现**：这是默认行为（用户要求默认不展开）
		if !expanded(session.ID) {
			return
		}
		for _, child := range children[session.ID] {
			walk(child)
		}
	}
	for _, root := range roots {
		walk(root)
	}
	// 兜底：把走不到的会话补到末尾（绝不丢会话）。
	//
	// 按设计这里应该永远是空的（子会话不能再派生，父会话总在列表里）。
	// 留着是为了档案被手工改坏时（比如父子互相引用）界面既不崩、也不丢项。
	for _, session := range sessions {
		if session == nil || reachable[session.ID] {
			continue
		}
		walk(session)
	}
	return out
}
