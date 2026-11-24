// Package skills 发现并读取项目与用户目录下的 skill。
//
// 约定（对齐 Agent Skills 规范）：
//
//	<root>/.agents/skills/<name>/SKILL.md
//	~/.agents/skills/<name>/SKILL.md
//
// **不扫 `skills/` 目录**：那是用户明确排除的（容易和项目里放脚本的
// `skills/` 混淆）。宁可少发现，也不要把无关目录当成 skill 读进来。
//
// 设计要点：**纯被动**。这个包只负责"有哪些 skill、内容是什么"，
// 完全不参与请求组装 —— 会话启动时不会把任何 skill 内容塞进系统提示或
// 历史。只有用户显式选了一个 skill，界面才把内容作为一条消息发出去。
package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// skillFileName 是每个 skill 的入口文件。
const skillFileName = "SKILL.md"

// Skill 是一个可用的 skill。
type Skill struct {
	// Name 是 skill 名（frontmatter 的 name，缺省用目录名）。
	Name string
	// Description 是一句话说明（frontmatter 的 description，可为空）。
	Description string
	// Dir 是 skill 目录（相对路径显示用绝对路径也行，这里存绝对路径，
	// 因为注入时要把"相对此目录的路径"告诉模型）。
	Dir string
	// Path 是 SKILL.md 的绝对路径。
	Path string
	// Scope 标明来源（"项目" / "用户"），界面上用来区分同名 skill。
	Scope string
}

// Source 是扫描的一个根目录。
type Source struct {
	// Dir 是 `.../.agents/skills` 这一级目录。
	Dir string
	// Scope 是显示用的来源名。
	Scope string
}

// DefaultSources 返回默认扫描位置：项目优先，其次用户目录。
//
// 项目在前：同名时项目版本覆盖用户版本（更贴近当前工作区的约定）。
func DefaultSources(workspace string) []Source {
	var out []Source
	if strings.TrimSpace(workspace) != "" {
		out = append(out, Source{
			Dir:   filepath.Join(workspace, ".agents", "skills"),
			Scope: "项目",
		})
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		out = append(out, Source{
			Dir:   filepath.Join(home, ".agents", "skills"),
			Scope: "用户",
		})
	}
	return out
}

// Discover 扫描所有来源，返回去重后的 skill 列表（按名字排序）。
//
// 同名 skill：**项目的优先**（Sources 的顺序决定），用户级的不再重复列出。
// 目录不存在不算错误（大多数项目没有 skill）。
func Discover(sources []Source) []Skill {
	seen := map[string]bool{}
	var out []Skill
	for _, source := range sources {
		for _, skill := range scanSource(source) {
			key := strings.ToLower(skill.Name)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, skill)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// scanSource 扫描一个来源目录下的直接子目录。
//
// 只认**直接子目录**里含 SKILL.md 的目录：递归太容易把 node_modules、
// 测试夹具里的 SKILL.md 扫进来，噪声远大于收益。
func scanSource(source Source) []Skill {
	entries, err := os.ReadDir(source.Dir)
	if err != nil {
		return nil
	}
	var out []Skill
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(source.Dir, entry.Name())
		path := filepath.Join(dir, skillFileName)
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			continue
		}
		skill := Skill{Dir: dir, Path: path, Scope: source.Scope, Name: entry.Name()}
		// frontmatter 只是补充说明：读失败不该让整个 skill 消失
		if meta, err := parseFrontmatter(path); err == nil {
			if meta.Name != "" {
				skill.Name = meta.Name
			}
			skill.Description = meta.Description
		}
		out = append(out, skill)
	}
	return out
}

// frontmatter 是 SKILL.md 头部的元数据。
type frontmatter struct {
	Name        string
	Description string
}

// parseFrontmatter 读取 SKILL.md 的 YAML frontmatter。
//
// 只支持 `key: value` 的单行形式：skill 的 frontmatter 实际只用得上
// name/description 两个简单字段，为此引入 YAML 依赖不值得。
// 遇到不认识的键直接忽略。
func parseFrontmatter(path string) (frontmatter, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return frontmatter{}, err
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return frontmatter{}, nil // 没有 frontmatter 是合法的
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return frontmatter{}, errors.New("frontmatter 未闭合")
	}
	var meta frontmatter
	for _, line := range strings.Split(rest[:end], "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		// 去掉可能的引号
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		switch strings.TrimSpace(key) {
		case "name":
			meta.Name = value
		case "description":
			meta.Description = value
		}
	}
	return meta, nil
}

// Load 读出某个 skill 的完整内容（SKILL.md 全文）。
//
// 注入时**原样发送**：frontmatter 也留着 —— 模型看到 name/description
// 有助于它理解这份指令的用途，删掉反而少了上下文。
func (s Skill) Load() (string, error) {
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return "", fmt.Errorf("读取 skill %s 失败：%w", s.Name, err)
	}
	text := string(raw)
	// 判空要看**正文**：只有 frontmatter 的文件等于没有指令，
	// 注入过去模型只会读到几行元数据，白占上下文。
	if strings.TrimSpace(stripFrontmatter(text)) == "" {
		return "", fmt.Errorf("skill %s 的 %s 没有正文内容", s.Name, skillFileName)
	}
	return text, nil
}

// stripFrontmatter 去掉头部的 YAML frontmatter（没有就原样返回）。
func stripFrontmatter(text string) string {
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return text
	}
	rest := normalized[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return text
	}
	return rest[end+len("\n---"):]
}

// Find 按名字（大小写不敏感）查找 skill。
func Find(list []Skill, name string) (Skill, bool) {
	for _, skill := range list {
		if strings.EqualFold(skill.Name, name) {
			return skill, true
		}
	}
	return Skill{}, false
}

// Prompt 组装要发给模型的那条消息。
//
// 结构：一句说明 + skill 目录 + 全文。
//
// 为什么要带目录：SKILL.md 里常用相对路径引用同目录的脚本/参考资料
// （"读 references/formats.md"、"运行 scripts/extract.sh"），
// 不告诉模型这些路径相对于哪里，它就找不到文件。
func (s Skill) Prompt() (string, error) {
	body, err := s.Load()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("使用技能「")
	b.WriteString(s.Name)
	b.WriteString("」。以下是它的完整说明")
	if s.Scope != "" {
		b.WriteString("（来源：")
		b.WriteString(s.Scope)
		b.WriteString("）")
	}
	b.WriteString("，其中引用的相对路径都相对于这个目录：\n")
	b.WriteString(s.Dir)
	b.WriteString("\n\n---\n\n")
	b.WriteString(body)
	return b.String(), nil
}
