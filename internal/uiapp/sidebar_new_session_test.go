package uiapp_test

import (
	"testing"
)

// 每个项目行的右侧都该有「在此项目新建会话」按钮。
//
// 用户的原话："创建会话放到 Project 的右边而不是顶部"，"每个 project 都有一个"。
// 为什么放行内：新建时才知道该归到哪个项目 —— 顶部那个只能落到默认工作区，
// 想在某项目下开新会话就得先切过去再点，多一步。
//
// 按钮**常驻**（不做悬停才显示）：藏起来的功能用户发现不了。
func TestSidebarGroupHasNewSessionButton(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	app.Current().SetWorkspaceForTest("/work/alpha")
	app.Current().Title = "alpha 会话"
	pump(tester, p)

	// 图标按钮不渲染可见文本，用 Label 定位
	if err := tester.Click("在此项目新建会话"); err != nil {
		t.Fatalf("项目行里应常驻新建按钮：%v（界面：%v）", err, tester.Texts())
	}
}

// 顶部不再有新建会话按钮（避免两个入口各走各的，行为不一致）。
func TestSidebarTopHasNoNewSessionButton(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	app.Current().SetWorkspaceForTest("/work/alpha")
	app.Current().Title = "alpha 会话"
	pump(tester, p)

	if err := tester.Click("新建会话"); err == nil {
		t.Fatalf("顶部不该再有新建会话按钮：%v", tester.Texts())
	}
}

// 项目行**不显示时间**（用户要求）。
//
// 判定要小心：会话行上也有相对时间（"刚刚"），不能直接数 Text。
// 这里比较"项目行文字"与"会话行文字"的差异 —— 项目行的标签只有项目名，
// 不该再跟一个时间后缀。
func TestSidebarGroupShowsNoTime(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	app.Current().SetWorkspaceForTest("/work/alpha")
	app.Current().Title = "alpha 会话"
	pump(tester, p)

	groupRect, ok := findRectContaining(tester, "alpha")
	if !ok {
		t.Fatalf("找不到项目行：%v", tester.Texts())
	}
	// 找出与项目名同一行（Y 轴重叠）的所有文字
	var sameLine []string
	for _, text := range tester.Texts() {
		rect, ok := tester.Find(text)
		if !ok {
			continue
		}
		if rect.Y+rect.H <= groupRect.Y || rect.Y >= groupRect.Y+groupRect.H {
			continue
		}
		sameLine = append(sameLine, text)
	}
	// 项目行上除了三角、项目名、按钮 label 之外，不该出现"刚刚""分钟前"这种
	for _, text := range sameLine {
		if isRelativeTime(text) {
			t.Fatalf("项目行不该显示时间，但在同一行找到了 %q（整行：%v）", text, sameLine)
		}
	}
}

// isRelativeTime 粗略判断一段文本是不是相对时间（"刚刚"、"3 分钟前"…）。
func isRelativeTime(text string) bool {
	switch text {
	case "刚刚", "昨天", "前天":
		return true
	}
	return len(text) > 2 && (contains(text, "分钟前") || contains(text, "小时前") ||
		contains(text, "天前") || contains(text, "周前"))
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// 点项目行里的新建按钮，新会话要归到**那个项目**，不是默认工作区。
//
// 这是整个改动的核心价值。
func TestSidebarGroupNewSessionUsesProjectWorkspace(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	app.Current().SetWorkspaceForTest("/work/alpha")
	app.Current().Title = "alpha 会话"
	pump(tester, p)

	if err := tester.Click("在此项目新建会话"); err != nil {
		t.Fatalf("点不到新建按钮：%v（界面：%v）", err, tester.Texts())
	}
	pump(tester, p)
	pump(tester, p)

	if got := len(app.SessionsForTest()); got < 2 {
		t.Fatalf("应新建出会话，实际只有 %d 个", got)
	}
	for _, session := range app.SessionsForTest() {
		if ws := session.WorkspaceForTest(); ws != "/work/alpha" {
			t.Fatalf("新会话的工作区应是 /work/alpha，实际 %q", ws)
		}
	}
}