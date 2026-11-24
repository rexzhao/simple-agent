package uiapp

import (
	"testing"

	"simple-agent/internal/host"
)

// 会话树的排序与折叠。
//
// 需求："如果可能，在界面左边做成父子关系，默认不展开"。

func mkSession(id, parent string) *Session {
	return &Session{
		Session:  &host.Session{ID: id, Title: id},
		parentID: parent,
	}
}

func idsOf(sessions []*Session) []string {
	out := make([]string, 0, len(sessions))
	for _, session := range sessions {
		out = append(out, session.ID)
	}
	return out
}

func equalIDs(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// **默认不展开**：子会话不出现在排序结果里。
func TestOrderHidesChildrenByDefault(t *testing.T) {
	sessions := []*Session{
		mkSession("parent", ""),
		mkSession("child", "parent"),
		mkSession("other", ""),
	}
	got := idsOf(orderSessionsByParent(sessions, func(string) bool { return false }))
	if !equalIDs(got, "parent", "other") {
		t.Fatalf("默认应收起子会话，实际 %v", got)
	}
}

// 展开后子会话紧跟在父会话之后。
func TestOrderShowsChildrenWhenExpanded(t *testing.T) {
	sessions := []*Session{
		mkSession("parent", ""),
		mkSession("child", "parent"),
		mkSession("other", ""),
	}
	expanded := func(id string) bool { return id == "parent" }
	got := idsOf(orderSessionsByParent(sessions, expanded))
	if !equalIDs(got, "parent", "child", "other") {
		t.Fatalf("子会话应紧跟父会话，实际 %v", got)
	}
}

// 多层：子会话再派生的挂在它下面（递归）。
func TestOrderNestedChildren(t *testing.T) {
	sessions := []*Session{
		mkSession("root", ""),
		mkSession("mid", "root"),
		mkSession("leaf", "mid"),
	}
	all := func(string) bool { return true }
	got := idsOf(orderSessionsByParent(sessions, all))
	if !equalIDs(got, "root", "mid", "leaf") {
		t.Fatalf("递归顺序不对：%v", got)
	}
}

// 父会话**不可见**时（被删/被搜索过滤），子会话当顶层显示 —— 不能丢。
func TestOrderOrphanShownAsRoot(t *testing.T) {
	sessions := []*Session{
		mkSession("orphan", "gone"),
		mkSession("other", ""),
	}
	got := idsOf(orderSessionsByParent(sessions, func(string) bool { return false }))
	if !equalIDs(got, "orphan", "other") {
		t.Fatalf("父会话不存在时子会话应按顶层显示：%v", got)
	}
}

// 自引用（脏数据）不能让界面死循环，也不能丢会话。
//
// 正常不会出现：子会话不挂 new_session 工具，生不出第二层。
// 这里只是防档案被手工改坏时界面别崩。
func TestOrderHandlesSelfParent(t *testing.T) {
	sessions := []*Session{
		mkSession("weird", "weird"),
		mkSession("other", ""),
	}
	all := func(string) bool { return true }
	got := idsOf(orderSessionsByParent(sessions, all))
	if len(got) != 2 {
		t.Fatalf("自我引用不该丢会话：%v", got)
	}
}

// 互相引用（脏数据）：不丢会话、不死循环。
//
// 设计上不允许出现环 —— 子会话没有 new_session 工具，不能再派生。
// 这条只验证"档案被改坏时界面不会崩"。
func TestOrderHandlesCycle(t *testing.T) {
	sessions := []*Session{
		mkSession("a", "b"),
		mkSession("b", "a"),
	}
	all := func(string) bool { return true }
	got := idsOf(orderSessionsByParent(sessions, all))
	if len(got) != 2 {
		t.Fatalf("环里的会话都要显示：%v", got)
	}
}

// 没有父子关系时顺序完全不变（不能打乱用户调整过的顺序）。
func TestOrderKeepsFlatOrder(t *testing.T) {
	sessions := []*Session{
		mkSession("c", ""),
		mkSession("a", ""),
		mkSession("b", ""),
	}
	got := idsOf(orderSessionsByParent(sessions, func(string) bool { return false }))
	if !equalIDs(got, "c", "a", "b") {
		t.Fatalf("扁平列表顺序不该变：%v", got)
	}
}

// toggleChildren 切换展开状态。
func TestToggleChildren(t *testing.T) {
	app := &App{}
	if app.childExpanded("x") {
		t.Fatal("默认不展开")
	}
	app.toggleChildren("x")
	if !app.childExpanded("x") {
		t.Fatal("切换后应展开")
	}
	app.toggleChildren("x")
	if app.childExpanded("x") {
		t.Fatal("再切换应收起")
	}
}
