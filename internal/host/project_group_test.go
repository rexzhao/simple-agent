package host_test

import (
	"testing"

	"simple-agent/internal/store"
)

// 还没登记进档案的新会话，也要归到**已有的那个项目**，不能另起一个同名分组。
//
// 真实踩到的：手机上新建议会，侧栏先出现两个 simple-agent，聊两句又合成一个。
// 原因：已登记的会话分组 key 是项目 UUID，没登记的用 workspace 路径，
// 两者显示名相同 → 两个同名分组。对话后 ensurePersisted 把会话登记进档案，
// 下一帧 key 变成 UUID，分组就合并了 —— 于是"闪现后消失"。
func TestUnregisteredSessionJoinsExistingProject(t *testing.T) {
	catalog := store.NewCatalog(t.TempDir())
	// 先建一个项目（模拟之前已经用过的目录）
	projectID, err := catalog.EnsureProject(`F:\work\alpha`)
	if err != nil {
		t.Fatal(err)
	}

	l := newHostWithCatalog(t, catalog)
	// 新建会话，**不产生任何内容**（不落盘、不登记）
	session := l.NewSession(`F:\work\alpha`)

	refs := l.Sessions()
	if len(refs) != 1 {
		t.Fatalf("应有 1 个会话，实际 %d", len(refs))
	}
	ref := refs[0]
	if ref.ID != session.ID {
		t.Fatalf("会话 id 不一致：%q != %q", ref.ID, session.ID)
	}
	// **关键**：即使还没登记，也要给出项目 ID —— 否则界面按 workspace 分组，
	// 就会出现第二个同名项目
	if ref.ProjectID != projectID {
		t.Fatalf("未登记的会话应归到已有项目 %q，实际 ProjectID=%q（空表示会另起同名分组）",
			projectID, ref.ProjectID)
	}
	if ref.ProjectName != "alpha" {
		t.Fatalf("项目名应是 alpha，实际 %q", ref.ProjectName)
	}
}

// 全新目录建会话：会**自动登记**一个新项目，ProjectID 不为空。
//
// 这是有意的行为变化（原来打算"没有项目档案就降级为空"）：
// 新会话一建出来就登记进档案，界面从一开始就有项目 ID，
// 不会出现"同一个项目两个分组"的中间态。
func TestNewSessionCreatesProjectWhenMissing(t *testing.T) {
	catalog := store.NewCatalog(t.TempDir())
	l := newHostWithCatalog(t, catalog)
	created := l.NewSession(`F:\brand-new-dir`)

	refs := l.Sessions()
	if len(refs) != 1 {
		t.Fatalf("应有 1 个会话，实际 %d", len(refs))
	}
	if refs[0].ProjectID == "" {
		t.Fatal("新会话应自动登记到一个新项目，ProjectID 不该为空")
	}
	if refs[0].ProjectName != "brand-new-dir" {
		t.Fatalf("项目名应取自目录末级，实际 %q", refs[0].ProjectName)
	}

	// 项目确实落进了档案
	projects, err := catalog.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].ID != refs[0].ProjectID {
		t.Fatalf("档案里应有该项目，实际 %+v", projects)
	}
	if len(projects[0].Sessions) != 1 || projects[0].Sessions[0].ID != created.ID {
		t.Fatalf("会话应已登记，实际 %+v", projects[0].Sessions)
	}
}

// 工作区路径的斜杠方向/尾部斜杠/大小写不同，也要认成同一个项目。
func TestUnregisteredSessionMatchesWorkspaceLoosely(t *testing.T) {
	catalog := store.NewCatalog(t.TempDir())
	projectID, err := catalog.EnsureProject(`F:\work\alpha`)
	if err != nil {
		t.Fatal(err)
	}

	l := newHostWithCatalog(t, catalog)
	// 各种等价写法
	for _, ws := range []string{`F:/work/alpha`, `F:\work\alpha\`, `f:\WORK\ALPHA`} {
		l2 := newHostWithCatalog(t, catalog)
		l2.NewSession(ws)
		refs := l2.Sessions()
		if len(refs) != 1 {
			t.Fatalf("%q：应有 1 个会话", ws)
		}
		if refs[0].ProjectID != projectID {
			t.Fatalf("%q 应归到项目 %q，实际 %q", ws, projectID, refs[0].ProjectID)
		}
	}
	_ = l
}