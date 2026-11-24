package skills_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"simple-agent/internal/skills"
)

// projectOnly 只扫项目内的 skills，**不碰用户目录**。
//
// 为什么不用 DefaultSources：它会扫 `~/.agents/skills`，而开发机上真的
// 有一堆用户级 skill（lark-* 等），测试会读到机器环境里的东西 ——
// 既不稳定，也可能让"应该没有 skill"的断言莫名其妙失败。
// DefaultSources 本身由 TestDefaultSourcesShape 单独验证。
func projectOnly(workspace string) []skills.Source {
	return []skills.Source{{
		Dir:   filepath.Join(workspace, ".agents", "skills"),
		Scope: "项目",
	}}
}

// writeSkill 造一个 skill 目录。
func writeSkill(t *testing.T, root, name, frontmatter, body string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := body
	if frontmatter != "" {
		content = "---\n" + frontmatter + "\n---\n\n" + body
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// DefaultSources 的形状：项目在前、用户在后，且**只有 .agents/skills**
// （不含 skills/，那是用户明确排除的）。
func TestDefaultSourcesShape(t *testing.T) {
	sources := skills.DefaultSources("/work/demo")
	if len(sources) < 2 {
		t.Fatalf("应有项目与用户两个来源：%+v", sources)
	}
	if !strings.HasSuffix(filepath.ToSlash(sources[0].Dir), "/work/demo/.agents/skills") {
		t.Fatalf("第一个应是项目的 .agents/skills：%q", sources[0].Dir)
	}
	if sources[0].Scope != "项目" {
		t.Fatalf("第一个来源应是项目：%q", sources[0].Scope)
	}
	if !strings.HasSuffix(filepath.ToSlash(sources[1].Dir), "/.agents/skills") {
		t.Fatalf("第二个应是用户的 .agents/skills：%q", sources[1].Dir)
	}
	// 项目来源优先（同名覆盖用户版本）
	for _, source := range sources {
		if strings.HasSuffix(filepath.ToSlash(source.Dir), "/skills") &&
			!strings.HasSuffix(filepath.ToSlash(source.Dir), ".agents/skills") {
			t.Fatalf("不该扫描非 .agents 的 skills 目录：%q", source.Dir)
		}
	}
}

// 扫描项目目录下的 .agents/skills。
func TestDiscoverProjectSkills(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, ".agents", "skills")
	writeSkill(t, root, "pdf-tools", "name: pdf-tools\ndescription: 处理 PDF", "# PDF\n读 references/x.md")

	list := skills.Discover(projectOnly(workspace))
	if len(list) != 1 {
		t.Fatalf("应发现 1 个 skill，实际 %d", len(list))
	}
	got := list[0]
	if got.Name != "pdf-tools" {
		t.Fatalf("名字不对：%q", got.Name)
	}
	if got.Description != "处理 PDF" {
		t.Fatalf("描述不对：%q", got.Description)
	}
	if got.Scope != "项目" {
		t.Fatalf("来源应是项目：%q", got.Scope)
	}
}

// **不扫 skills/ 目录**（用户明确排除）。
func TestDiscoverIgnoresPlainSkillsDir(t *testing.T) {
	workspace := t.TempDir()
	// 放一个 skills/ 目录（不是 .agents/skills）
	writeSkill(t, filepath.Join(workspace, "skills"), "should-not-appear", "name: x", "body")

	list := skills.Discover(projectOnly(workspace))
	for _, skill := range list {
		if skill.Name == "should-not-appear" {
			t.Fatal("不该扫描 skills/ 目录")
		}
	}
}

// 没有 frontmatter 时用目录名（合法，不是错误）。
func TestDiscoverWithoutFrontmatter(t *testing.T) {
	workspace := t.TempDir()
	writeSkill(t, filepath.Join(workspace, ".agents", "skills"), "my-skill", "", "# 正文")

	list := skills.Discover(projectOnly(workspace))
	if len(list) != 1 || list[0].Name != "my-skill" {
		t.Fatalf("应回退到目录名：%+v", list)
	}
}

// 同名时项目版优先，用户版不重复列出。
func TestProjectWinsOverUser(t *testing.T) {
	workspace := t.TempDir()
	projectRoot := filepath.Join(workspace, ".agents", "skills")
	userRoot := t.TempDir()

	writeSkill(t, projectRoot, "dup", "name: dup\ndescription: 项目版", "项目内容")
	writeSkill(t, userRoot, "dup", "name: dup\ndescription: 用户版", "用户内容")

	list := skills.Discover([]skills.Source{
		{Dir: projectRoot, Scope: "项目"},
		{Dir: userRoot, Scope: "用户"},
	})
	if len(list) != 1 {
		t.Fatalf("同名应去重成 1 个，实际 %d", len(list))
	}
	if list[0].Description != "项目版" {
		t.Fatalf("应取项目版：%q", list[0].Description)
	}
}

// 只认直接子目录里含 SKILL.md 的：嵌套太深的不算（避免扫到 node_modules）。
func TestDiscoverOnlyDirectChildren(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, ".agents", "skills")
	// 深层嵌套
	deep := filepath.Join(root, "outer", "inner")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(deep, "SKILL.md"), []byte("---\nname: deep\n---\n"), 0o644)

	list := skills.Discover(projectOnly(workspace))
	if len(list) != 0 {
		t.Fatalf("嵌套的 SKILL.md 不该被发现：%+v", list)
	}
}

// 注入的消息要带目录（SKILL.md 里常用相对路径引用同目录文件）。
func TestPromptIncludesDirAndBody(t *testing.T) {
	workspace := t.TempDir()
	dir := writeSkill(t, filepath.Join(workspace, ".agents", "skills"),
		"pdf-tools", "name: pdf-tools\ndescription: d", "# PDF tools\n\n读 references/formats.md")

	list := skills.Discover(projectOnly(workspace))
	prompt, err := list[0].Prompt()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "pdf-tools") {
		t.Fatalf("应说明用了哪个 skill：%q", prompt)
	}
	if !strings.Contains(prompt, dir) {
		t.Fatalf("应带 skill 目录（相对路径的基准）：%q", prompt)
	}
	if !strings.Contains(prompt, "读 references/formats.md") {
		t.Fatalf("应包含 SKILL.md 全文：%q", prompt)
	}
}

// 空 SKILL.md 要报错（而不是注入一个空提示）。
func TestLoadRejectsEmpty(t *testing.T) {
	workspace := t.TempDir()
	writeSkill(t, filepath.Join(workspace, ".agents", "skills"), "empty", "name: empty", "   ")

	list := skills.Discover(projectOnly(workspace))
	if _, err := list[0].Load(); err == nil {
		t.Fatal("空 skill 应报错")
	}
}

// 目录不存在时是空列表，不是错误。
func TestDiscoverMissingDirIsEmpty(t *testing.T) {
	list := skills.Discover(projectOnly(t.TempDir()))
	if len(list) != 0 {
		t.Fatalf("应没有 skill：%+v", list)
	}
}

// Find 大小写不敏感。
func TestFind(t *testing.T) {
	list := []skills.Skill{{Name: "PDF-Tools"}}
	if _, ok := skills.Find(list, "pdf-tools"); !ok {
		t.Fatal("查找应忽略大小写")
	}
	if _, ok := skills.Find(list, "nope"); ok {
		t.Fatal("不该找到")
	}
}
