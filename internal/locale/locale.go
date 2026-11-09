// Package locale 是界面语言的唯一来源：语言判断 + 中英双语文案表。
//
// 设计（方案 §12 第 45 条）：
//   - key 命名：dsh（deepseek-harness）里有的文案**直接沿用它的 key**
//     （`session.new.label`、`time.now`、`message.turnProcess.took`、
//     `message.stepProcess.*`），我们自己的用前缀（`composer.send`、`sidebar.search`…）；
//   - 两张表 `zhTable` / `enTable`：**key 必须完全一致、值不能为空**，由测试守着；
//   - 带参数的文案在两种语言里**占位符个数必须一致**（同样由测试守着）；
//   - 查不到时回退 en、再回退 key 本身：这只是兜底，不能拿来掩盖漏翻。
package locale

import (
	"fmt"
	"sort"
	"strings"
)

// Lang 是界面语言。
type Lang string

const (
	// ZH 是简体中文。
	ZH Lang = "zh"
	// EN 是英文（其他语言的默认）。
	EN Lang = "en"
)

// Parse 解析配置里的语言值：""/"auto" 表示跟随系统，其他只认 zh/en。
func Parse(value string) (Lang, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "auto":
		return Detect(), true
	case "zh", "zh-cn", "zh_cn", "chinese":
		return ZH, true
	case "en", "en-us", "en_us", "english":
		return EN, true
	default:
		return EN, false
	}
}

// 进程级默认语言：给拿不到 App 的地方用（图片 URL 校验、日志等）。
// App 启动时用 SetCurrent 同步一次；切换语言时再同步。
var current = Detect()

// Current 返回进程级默认语言。
func Current() Lang { return current }

// SetCurrent 设置进程级默认语言。
func SetCurrent(l Lang) {
	if l != ZH && l != EN {
		return
	}
	current = l
}

// Tr 用进程级默认语言查表（App 里请用 Lang.Tr，避免多语言环境下串味）。
func Tr(key string, args ...any) string { return current.Tr(key, args...) }

// Tr 查表并格式化。args 按 fmt.Sprintf 的占位符填充。
func (l Lang) Tr(key string, args ...any) string {
	text := lookup(l, key)
	if text == "" {
		return key
	}
	if len(args) == 0 {
		return text
	}
	return fmt.Sprintf(text, args...)
}

// lookup 返回某个 key 在给定语言下的文案：先查该语言，再回退另一种语言。
func lookup(l Lang, key string) string {
	if l == EN {
		if text, ok := enTable[key]; ok {
			return text
		}
		return zhTable[key]
	}
	if text, ok := zhTable[key]; ok {
		return text
	}
	return enTable[key]
}

// Keys 返回全部 key（按字典序；测试与文档用）。
func Keys() []string {
	keys := make([]string, 0, len(zhTable))
	for key := range zhTable {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Table 返回某种语言的文案表（只读用途：测试与文档生成）。
func Table(l Lang) map[string]string {
	if l == EN {
		return enTable
	}
	return zhTable
}
