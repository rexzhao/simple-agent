package locale_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"simple-agent/internal/locale"
)

// 表的完整性：两种语言的 key 集合一致、值非空、格式占位符个数一致。
//
// 运行时会回退（缺 key → en，再缺 → key 本身），但那是兜底，不能拿来掩盖漏翻。
func TestTablesAreComplete(t *testing.T) {
	zh := locale.Table(locale.ZH)
	en := locale.Table(locale.EN)

	for key, text := range zh {
		if strings.TrimSpace(text) == "" {
			t.Errorf("zh 文案为空：%s", key)
		}
		other, ok := en[key]
		if !ok {
			t.Errorf("en 缺少 key：%s", key)
			continue
		}
		if strings.TrimSpace(other) == "" {
			t.Errorf("en 文案为空：%s", key)
		}
		if got, want := countVerbs(other), countVerbs(text); got != want {
			t.Errorf("%s 的占位符个数不一致：zh %d 个、en %d 个（zh=%q en=%q）",
				key, want, got, text, other)
		}
	}
	for key := range en {
		if _, ok := zh[key]; !ok {
			t.Errorf("zh 缺少 key：%s（en 有）", key)
		}
	}
}

// countVerbs 数 fmt 占位符个数（%d/%s/%v/%q/%f/%x…，忽略 %%）。
func countVerbs(text string) int {
	count := 0
	for i := 0; i < len(text); i++ {
		if text[i] != '%' {
			continue
		}
		if i+1 < len(text) && text[i+1] == '%' {
			i++ // %% 不是占位符
			continue
		}
		count++
		i++ // 跳过动词本身（%02d 这类宽度修饰不影响计数）
		for i < len(text) && strings.ContainsRune("0123456789+-.# ", rune(text[i])) {
			i++
		}
	}
	return count
}

// 代码里用到的 key 必须在表里：扫 `tr("…")` / `T("…")` 这类调用的字面量。
func TestEveryUsedKeyExists(t *testing.T) {
	pattern := regexp.MustCompile(`(?:\btr|\bTr|\bT|\ba\.tr)\(\s*"([^"]+)"`)
	known := locale.Table(locale.ZH)
	root := filepath.Join("..", "..", "internal")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, match := range pattern.FindAllStringSubmatch(string(data), -1) {
			key := match[1]
			if strings.Contains(key, "%") || strings.Contains(key, " ") {
				continue // 不是 locale key（普通格式化字符串）
			}
			if _, ok := known[key]; !ok {
				t.Errorf("%s 里用到了表里没有的 key：%q", path, key)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// 无障碍名（`Label`/`Placeholder`/`Tooltip`）的两种误用，都在这里守着。
//
// 一、**不能用带占位符的模板**：这些 API 只接受成品字符串，不经过 fmt 格式化。
// 传 `tr("composer.usage")` 这种带 %s 的模板，读屏会念出「上下文 %s / %s」。
//
// 二、**`ui.Text` 不要设 Label**：mygo 的 a11y 树里，元素一旦有 Label 就用它
// 顶替可见文本（ui/runtime.go 的 label 取值），文本本身反而不再暴露给读屏——
// 而文本的无障碍名本来就该是它自己的文字。设了只会帮倒忙，且让测试查不到文案。
// 这一条不在这里扫（要判断元素类型，交给界面测试），这里只守第一条。
func TestAccessibilityLabelsHaveNoVerbs(t *testing.T) {
	// 只扫「直接传给 Label/Placeholder/Tooltip 的 tr 调用」这一种形态；
	// 带参数的正确用法（tr(key, args…)）不受影响。
	pattern := regexp.MustCompile(`(?:Label|Placeholder|Tooltip)\(\s*a?\.?tr\(\s*"([^"]+)"\s*\)\s*\)`)
	root := filepath.Join("..", "..", "internal")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, match := range pattern.FindAllStringSubmatch(string(data), -1) {
			key := match[1]
			text, ok := locale.Table(locale.ZH)[key]
			if !ok {
				continue // 缺 key 由 TestEveryUsedKeyExists 负责
			}
			if countVerbs(text) > 0 {
				t.Errorf("%s 把带占位符的 %q 直接传给了 Label/Placeholder/Tooltip"+
					"（它不会做格式化，读屏会念出原文）：%q", path, key, text)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Detect 与 Parse 的基本行为。
func TestLanguageDetection(t *testing.T) {
	// 显式值优先
	if lang, ok := locale.Parse("en"); !ok || lang != locale.EN {
		t.Fatalf("en 应解析成 EN：%v %v", lang, ok)
	}
	if lang, ok := locale.Parse("zh"); !ok || lang != locale.ZH {
		t.Fatalf("zh 应解析成 ZH：%v %v", lang, ok)
	}
	if _, ok := locale.Parse("klingon"); ok {
		t.Fatal("未知语言应返回 ok=false（由调用方回落到 auto）")
	}

	// auto（""）跟随系统：默认不是 zh 就是 en
	if lang, ok := locale.Parse(""); !ok || (lang != locale.ZH && lang != locale.EN) {
		t.Fatalf("auto 应返回 zh 或 en：%v %v", lang, ok)
	}

	// 查表：未知 key 原样返回，参数按 fmt 填充
	if got := locale.ZH.Tr("common.send.nonexistent"); got != "common.send.nonexistent" {
		t.Fatalf("未知 key 应原样返回：%q", got)
	}
	if got := locale.EN.Tr("session.pruned", 3); !strings.Contains(got, "3") {
		t.Fatalf("占位符应被填充：%q", got)
	}
}
