package timeline

import (
	"strings"

	"simple-agent/internal/locale"
	"simple-agent/internal/message"
	"simple-agent/internal/render"
)

// Activity 是过程组的活动分类，照抄 deepseek-harness 的 step-process。
type Activity int

const (
	ActivityThinking Activity = iota
	ActivityRead
	ActivityReadImage
	ActivitySearch
	ActivityWrite
	ActivityEdit
	ActivityCommands
	ActivityCode
	ActivityWebSearch
	ActivityWebFetch
	ActivitySubagents
	ActivityPlan
	ActivityQuestions
	ActivityTools
)

// activityKeys 是每个分类的 locale key：[进行中, 已完成]。
// 直接沿用 dsh 的 key（`message.stepProcess.<kind>` / `…done.<kind>`）。
var activityKeys = map[Activity][2]string{
	ActivityThinking:  {"message.stepProcess.thinking", "message.stepProcess.done.thinking"},
	ActivityRead:      {"message.stepProcess.read", "message.stepProcess.done.read"},
	ActivityReadImage: {"message.stepProcess.readImage", "message.stepProcess.done.readImage"},
	ActivitySearch:    {"message.stepProcess.search", "message.stepProcess.done.search"},
	ActivityWrite:     {"message.stepProcess.write", "message.stepProcess.done.write"},
	ActivityEdit:      {"message.stepProcess.edit", "message.stepProcess.done.edit"},
	ActivityCommands:  {"message.stepProcess.commands", "message.stepProcess.done.commands"},
	ActivityCode:      {"message.stepProcess.code", "message.stepProcess.done.code"},
	ActivityWebSearch: {"message.stepProcess.webSearch", "message.stepProcess.done.webSearch"},
	ActivityWebFetch:  {"message.stepProcess.webFetch", "message.stepProcess.done.webFetch"},
	ActivitySubagents: {"message.stepProcess.subagents", "message.stepProcess.done.subagents"},
	ActivityPlan:      {"message.stepProcess.plan", "message.stepProcess.done.plan"},
	ActivityQuestions: {"message.stepProcess.questions", "message.stepProcess.done.questions"},
	ActivityTools:     {"message.stepProcess.tools", "message.stepProcess.done.tools"},
}

// ActivityOf 把工具名归到分类（与 dsh 的 activity() 对齐）。
func ActivityOf(name string) Activity {
	switch name {
	case "read":
		return ActivityRead
	case "read_image":
		return ActivityReadImage
	case "grep", "glob":
		return ActivitySearch
	case "write":
		return ActivityWrite
	case "edit", "apply_patch":
		return ActivityEdit
	case "bash", "pwsh", "exec_command", "write_stdin":
		return ActivityCommands
	case "run_code":
		return ActivityCode
	case "web_search":
		return ActivityWebSearch
	case "web_fetch":
		return ActivityWebFetch
	case "subagent":
		return ActivitySubagents
	case "todo_write", "create_goal", "update_goal", "get_goal":
		return ActivityPlan
	case "ask_user_question", "request_user_input":
		return ActivityQuestions
	}
	return ActivityTools
}

// ActivityLabel 返回分类的显示名（进行中 / 已完成）。
func ActivityLabel(activity Activity, done bool, lang locale.Lang) string {
	keys, ok := activityKeys[activity]
	if !ok {
		keys = activityKeys[ActivityTools]
	}
	index := 0
	if done {
		index = 1
	}
	return lang.Tr(keys[index])
}

// GroupTitle 生成过程组标题。
//
// 照抄 deepseek-harness：
//   - 进行中：显示当前活动，「正在读取文件 · docs/plan.md」；
//   - 已完成：把出现过的活动按调用次数排序（次数相同按首次出现），取前三项
//     拼成「已读取文件并搜索代码」，四项以上加「等」。
//
// 两种都不显示计数——展开后就能数，标题只表达"干了什么"。
func GroupTitle(refs []Ref, streaming bool, lang locale.Lang) string {
	counts := make(map[Activity]int, 4)
	var order []Activity
	add := func(kind Activity) {
		if _, ok := counts[kind]; !ok {
			order = append(order, kind)
		}
		counts[kind]++
	}
	for _, ref := range refs {
		part := ref.Part()
		switch part.MediaType {
		case render.MediaReasoning:
			add(ActivityThinking)
		case render.MediaToolUse:
			data, ok := partData(part)
			if !ok {
				continue
			}
			add(ActivityOf(stringOf(data, "name")))
		}
	}
	if len(order) == 0 {
		return ""
	}

	if streaming {
		live, detail := CurrentActivity(refs)
		if live < 0 {
			return ""
		}
		title := ActivityLabel(live, false, lang)
		if detail != "" {
			title += " · " + detail
		}
		return title
	}

	ranked := sortedActivities(counts, order)
	shown := ranked
	if len(shown) > 3 {
		shown = shown[:3]
	}
	return joinActivityTitles(shown, len(ranked) > 3, lang)
}

// GroupActivity 返回这一组当前（或最后一次）活动的分类，供标题前的图标使用。
func GroupActivity(refs []Ref, streaming bool) Activity {
	kind, _ := CurrentActivity(refs)
	if kind >= 0 {
		return kind
	}
	// 兜底：看每个 part 的类型
	for index := len(refs) - 1; index >= 0; index-- {
		part := refs[index].Part()
		switch part.MediaType {
		case render.MediaReasoning:
			return ActivityThinking
		case render.MediaToolUse, render.MediaToolResult:
			if data, ok := partData(part); ok {
				return ActivityOf(stringOf(data, "name"))
			}
		}
	}
	return ActivityTools
}

// CurrentActivity 找这一组**当前正在做**的事及其一行摘要（仅进行中使用）。
func CurrentActivity(refs []Ref) (Activity, string) {
	for index := len(refs) - 1; index >= 0; index-- {
		part := refs[index].Part()
		switch part.MediaType {
		case render.MediaReasoning:
			if text, ok := part.AsText(); ok {
				if preview := LastLine(text, 40); preview != "" {
					return ActivityThinking, preview
				}
			}
			return ActivityThinking, ""
		case render.MediaToolUse:
			data, ok := partData(part)
			if !ok {
				continue
			}
			return ActivityOf(stringOf(data, "name")), ToolArgSummary(data)
		case render.MediaToolResult:
			data, ok := partData(part)
			if !ok {
				continue
			}
			return ActivityOf(stringOf(data, "name")), ""
		}
	}
	return -1, ""
}

// joinActivityTitles 把已完成的分类拼成一句话。
//
// 中文：第二项起去掉共有的「已」前缀，用「并」「，」连接，多于三项加「等」；
// 英文：第二项起首字母小写，用「, 」与「 and 」连接，多于三项加「, etc.」。
// （dsh 的 `processTitle` 也是这个规则。）
func joinActivityTitles(kinds []Activity, more bool, lang locale.Lang) string {
	labels := make([]string, 0, len(kinds))
	for index, kind := range kinds {
		label := ActivityLabel(kind, true, lang)
		if index > 0 {
			if lang == locale.EN {
				label = lowerFirst(label)
			} else {
				label = strings.TrimPrefix(label, "已")
			}
		}
		labels = append(labels, label)
	}
	if len(labels) == 0 {
		return ""
	}
	title := labels[0]
	if len(labels) > 1 {
		if lang == locale.EN {
			title += ", " + strings.Join(labels[1:len(labels)-1], ", ")
			if len(labels) > 2 {
				title += ","
			}
			title += lang.Tr("message.stepProcess.join", labels[len(labels)-1])
		} else if len(labels) == 2 {
			title += lang.Tr("message.stepProcess.join", labels[1])
		} else {
			title += "，" + strings.Join(labels[1:], "，")
		}
	}
	if more {
		title = lang.Tr("message.stepProcess.more", title)
	}
	return title
}

// lowerFirst 把首字母改小写（英文拼接用；CJK 不受影响）。
func lowerFirst(text string) string {
	runes := []rune(text)
	if len(runes) == 0 {
		return text
	}
	if runes[0] >= 'A' && runes[0] <= 'Z' {
		runes[0] = runes[0] - 'A' + 'a'
	}
	return string(runes)
}

// argSummaryKeys 是生成一行摘要时优先看的参数（与 dsh 的 detail 键顺序一致）。
var argSummaryKeys = []string{
	"title", "command", "cmd", "queries", "query", "pattern",
	"url", "file_path", "path", "target", "content",
}

// ToolArgSummary 生成工具调用的一行参数摘要。
//
// 参数还在流式接收时也要能显示（dsh 同样边收边显示），所以除了完整的 args
// 之外还会从半截 JSON 里抠值。
func ToolArgSummary(data map[string]any) string {
	if args, ok := data["args"].(map[string]any); ok {
		for _, key := range argSummaryKeys {
			if value, ok := args[key].(string); ok && value != "" {
				return ClipLine(value, 60)
			}
		}
	}
	if raw := stringOf(data, "raw_args"); raw != "" {
		if value := partialArgValue(raw); value != "" {
			return ClipLine(value, 60)
		}
	}
	return ""
}

// partialArgValue 从可能还没收完的 JSON 里取出第一个摘要参数的字符串值。
func partialArgValue(raw string) string {
	for _, key := range argSummaryKeys {
		if value := jsonValueFor(raw, key); value != "" {
			return value
		}
	}
	return ""
}

// jsonValueFor 在 raw 里找 "key": <value> 的值（value 可以还没收完）。
func jsonValueFor(raw, key string) string {
	needle := `"` + key + `"`
	search := 0
	for {
		found := strings.Index(raw[search:], needle)
		if found < 0 {
			return ""
		}
		pos := search + found + len(needle)
		search = pos
		rest := strings.TrimLeft(raw[pos:], " \t\r\n")
		if !strings.HasPrefix(rest, ":") {
			continue
		}
		rest = strings.TrimLeft(rest[1:], " \t\r\n")
		if rest == "" {
			return ""
		}
		if rest[0] == '"' {
			return unquotePartial(rest)
		}
		// 数字 / 布尔 / null：读到分隔符为止
		end := strings.IndexAny(rest, ",}]")
		if end < 0 {
			return ""
		}
		value := strings.TrimSpace(rest[:end])
		if value == "" || value == "null" {
			return ""
		}
		return value
	}
}

// unquotePartial 解析从引号开始的字符串字面量；没收完也算（返回已收到的部分）。
func unquotePartial(rest string) string {
	var builder strings.Builder
	for index := 1; index < len(rest); index++ {
		ch := rest[index]
		if ch == '\\' && index+1 < len(rest) {
			index++
			switch rest[index] {
			case 'n':
				builder.WriteByte('\n')
			case 't':
				builder.WriteByte('\t')
			default:
				builder.WriteByte(rest[index])
			}
			continue
		}
		if ch == '"' {
			break // 值收完了
		}
		builder.WriteByte(ch)
	}
	return builder.String()
}

// LastLine 取最后一行非空文本并截断（流式预览用）。
func LastLine(text string, limit int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		runes := []rune(line)
		if len(runes) > limit {
			return "…" + string(runes[len(runes)-limit:])
		}
		return line
	}
	return ""
}

// ClipLine 把单行文本截断到指定长度。
func ClipLine(text string, limit int) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\n", " "), "\r", " ")
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return text
}

// partData 取 Data part 的对象视图。
func partData(part message.Part) (map[string]any, bool) {
	dataPart, ok := part.Content.(message.DataPart)
	if !ok {
		return nil, false
	}
	data, ok := dataPart.Data.(map[string]any)
	return data, ok
}

func stringOf(data map[string]any, key string) string {
	value, _ := data[key].(string)
	return value
}
