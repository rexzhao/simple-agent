package uiapp_test

import (
	"strings"
	"testing"
)

// 侧栏分组按**项目档案**，不再拿会话的 workspace 反推。
//
// 反推的毛病：只有名字没有先后 —— 项目顺序由 projects.json 决定，
// "移除的项目"也表达不了。现在后端给 ProjectID/ProjectName，
// 界面照它分组与排序。
func TestSidebarGroupsByProjectRecord(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)

	first := app.Current()
	first.Title = "会话一"
	second := app.NewSession()
	second.Title = "会话二"
	pump(tester, p)
	pump(tester, p)

	// 两个会话挂在同一个项目下（模拟后端同步的档案数据）
	// pump 之后设：syncSessions 每帧从后端刷新，先设会被冲掉
	first.SetProjectForTest("proj-a", "我的项目")
	second.SetProjectForTest("proj-a", "我的项目")
	tester.Frame()

	// 侧栏应只有**一个**项目行，名字是档案里的
	count := 0
	for _, text := range tester.Texts() {
		if text == "我的项目" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("同一项目下的两个会话应合成一个项目行，实际出现 %d 次：%v",
			count, tester.Texts())
	}
}

// 两个不同项目要分开成两行，且顺序跟档案顺序（先出现的项目在上）。
func TestSidebarProjectOrderFollowsRecords(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)

	first := app.Current()
	first.Title = "甲会话"
	second := app.NewSession()
	second.Title = "乙会话"
	pump(tester, p)
	pump(tester, p)

	// 设置必须在 pump **之后**：syncSessions 每次都从后端刷新项目信息，
	// 先设会被冲掉。设完再推一帧让它渲染出来。
	first.SetProjectForTest("proj-1", "项目甲")
	second.SetProjectForTest("proj-2", "项目乙")
	tester.Frame()

	texts := tester.Texts()
	posA, posB := -1, -1
	for i, text := range texts {
		if text == "项目甲" {
			posA = i
		}
		if text == "项目乙" {
			posB = i
		}
	}
	if posA < 0 || posB < 0 {
		t.Fatalf("两个项目行都应出现：%v", texts)
	}
	if posA > posB {
		t.Fatalf("项目顺序应跟档案一致（甲在前），实际甲=%d 乙=%d：%v",
			posA, posB, texts)
	}
}

// 会话标题旁的复制按钮把 project 与 session 的 id 一起复制。
//
// 用户的要求："显示会话名的右边，加个复制按钮，可以复制 project 和
// session 的 id，方便沟通"。
func TestSidebarCopySessionIDs(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	session := app.Current()
	session.Title = "要复制的会话"
	pump(tester, p)
	// 设项目信息要在 pump 之后（syncSessions 会刷掉）
	session.SetProjectForTest("proj-abc123", "项目")
	tester.Frame()

	// 悬停会话行让复制按钮可见
	rowRect, ok := findRectContaining(tester, "要复制的会话")
	if !ok {
		t.Fatalf("找不到会话行：%v", tester.Texts())
	}
	tester.Move(rowRect.X+rowRect.W/2, rowRect.Y+rowRect.H/2)
	tester.Frame()
	pump(tester, p)

	if err := tester.Click(locCopyIDs); err != nil {
		t.Fatalf("点不到复制按钮：%v（界面：%v）", err, tester.Texts())
	}
	pump(tester, p)

	copied := tester.Clipboard()
	if !strings.Contains(copied, "project: proj-abc123") {
		t.Fatalf("应复制项目 id，实际剪贴板内容：%q", copied)
	}
	if !strings.Contains(copied, "session: "+session.ID) {
		t.Fatalf("应复制会话 id，实际剪贴板内容：%q", copied)
	}
}

// 复制按钮的**结构要常驻**：不悬停时透明但不消失。
//
// 原因：mygo 的原生菜单靠元素 id 匹配，兄弟元素在悬停时出现/消失会让
// 后面的「⋯」id 位移，菜单就选不中（侧栏会话行踩过这个坑）。
func TestSidebarCopyButtonKeepsStructureWhenIdle(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	app.Current().Title = "空闲会话"
	pump(tester, p)
	app.Current().SetProjectForTest("proj-idle", "项目")
	tester.Frame()

	// 指针在别处（不悬停）
	tester.Move(5, 5)
	tester.Frame()
	pump(tester, p)

	// 元素仍在树里：能点中就是证据
	if err := tester.Click(locCopyIDs); err != nil {
		t.Fatalf("复制按钮应常驻（不悬停也在），实际点不到：%v", err)
	}
}