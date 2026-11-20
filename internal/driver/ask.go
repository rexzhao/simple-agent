package driver

import (
	"fmt"
	"strings"

	"simple-agent/internal/tools"
)

// AskAction 是用户对提问组的处理方式。
type AskAction int

const (
	// AskSubmit 提交当前选中的选项。
	AskSubmit AskAction = iota
	// AskDismiss 跳过全部问题。
	AskDismiss
	// AskCustom 把输入框里的文字作为答案提交。
	AskCustom
)

// AskGroup 是一组待回答的提问（对应一次 ask_user_question 调用）。
//
// 状态放在编排层而不是 UI 层：挂起状态必须按 (会话, 消息, call) 区分，
// 作答同时要改会话数据与忙闲状态——那是 driver 的职责。
type AskGroup struct {
	MessageID string
	CallID    string
	Questions []tools.QuestionSpec

	// selected 是每题已选中的选项下标。
	selected []map[int]struct{}
	// custom 是每题自己的手写答案（用户在该题输入框里写的内容）。
	custom []string
	// customActive 表示该题的答案此刻用手写文字（false 时文字只是留在框里，
	// 用户可以改选预设选项而不丢自己写的内容，随时能改回来）。
	customActive []bool
}

// NewAskGroup 构造提问组。
func NewAskGroup(messageID, callID string, questions []tools.QuestionSpec) *AskGroup {
	selected := make([]map[int]struct{}, len(questions))
	for i := range selected {
		selected[i] = map[int]struct{}{}
	}
	return &AskGroup{
		MessageID:    messageID,
		CallID:       callID,
		Questions:    questions,
		selected:     selected,
		custom:       make([]string, len(questions)),
		customActive: make([]bool, len(questions)),
	}
}

// Toggle 切换某个选项：单选互斥，多选叠加（再次点击取消）。
func (g *AskGroup) Toggle(questionIndex, optionIndex int) {
	if questionIndex < 0 || questionIndex >= len(g.Questions) {
		return
	}
	if optionIndex < 0 || optionIndex >= len(g.Questions[questionIndex].Options) {
		return
	}
	picked := g.selected[questionIndex]
	if g.Questions[questionIndex].MultiSelect {
		if _, ok := picked[optionIndex]; ok {
			delete(picked, optionIndex)
			// 取消到一项不剩，才把手写文字重新当成答案（与单选路径一致）
			if len(picked) == 0 {
				g.restoreCustom(questionIndex)
			}
			return
		}
		picked[optionIndex] = struct{}{}
		// **多选也要清掉"以文字作答"的标记**。
		//
		// 少了这一步会丢用户的选择：先写自定义文字（customActive=true）、
		// 再点选项时，customActive 仍是 true，answers() 里
		// `case g.CustomActive(index)` 优先命中，于是答案只剩那段文字，
		// 勾选的选项被完全忽略 —— 模型看到的就是"用户只写了文字"。
		//
		// 单选路径一直有这句，多选漏了。
		if questionIndex < len(g.customActive) {
			g.customActive[questionIndex] = false
		}
		return
	}
	clear(picked)
	picked[optionIndex] = struct{}{}
	// 选了选项就以选项作答，但**不删**已写的文字：用户可以反悔改回文字
	if questionIndex < len(g.customActive) {
		g.customActive[questionIndex] = false
	}
	if len(picked) == 0 {
		g.restoreCustom(questionIndex)
	}
}

// SetCustom 记录第 index 题的手写答案。
//
// 文字非空时清掉该题的选项：用户写了内容就说明这一题按文字作答，
// 界面上的选择标记也应随之消失。
func (g *AskGroup) SetCustom(index int, text string) {
	if index < 0 || index >= len(g.custom) {
		return
	}
	text = strings.TrimSpace(text)
	if text == g.custom[index] {
		return
	}
	g.custom[index] = text
	if index < len(g.customActive) {
		g.customActive[index] = text != ""
	}
	if text != "" {
		// 开始写文字就以文字作答，取消该题已选选项
		clear(g.selected[index])
	}
}

// restoreCustom 在选项被全部取消后，把手写文字重新作为答案。
func (g *AskGroup) restoreCustom(index int) {
	if index < 0 || index >= len(g.customActive) {
		return
	}
	g.customActive[index] = g.custom[index] != ""
}

// CustomActive 报告某题此刻是否以手写文字作答。
func (g *AskGroup) CustomActive(index int) bool {
	if index < 0 || index >= len(g.customActive) {
		return false
	}
	return g.customActive[index]
}

// Custom 返回第 index 题的手写答案。
func (g *AskGroup) Custom(index int) string {
	if index < 0 || index >= len(g.custom) {
		return ""
	}
	return g.custom[index]
}

// Answered 报告某题是否已作答（选了选项或写了文字）。
// Answered 报告某题是否已有答案（选了选项或正在以文字作答）。
func (g *AskGroup) Answered(index int) bool {
	return g.CustomActive(index) || g.picked(index) > 0
}

// AllAnswered 报告**每一题**是否都已有答案。
//
// 用途：多个问题时，「提交」要等全部答完才可用 —— 否则用户容易漏答一题
// 就提交，模型拿到一个空答案还要反问一次，白跑一个回合。
//
// 没有选项的开放题也算"未答"：那种题只能靠输入框作答。
func (g *AskGroup) AllAnswered() bool {
	for index := range g.Questions {
		if !g.Answered(index) {
			return false
		}
	}
	return true
}

// AnsweredCount 返回已作答的题数（界面显示进度用）。
func (g *AskGroup) AnsweredCount() int {
	count := 0
	for index := range g.Questions {
		if g.Answered(index) {
			count++
		}
	}
	return count
}

// picked 返回某题选了几个选项。
func (g *AskGroup) picked(index int) int {
	if index < 0 || index >= len(g.selected) {
		return 0
	}
	return len(g.selected[index])
}

// Selected 报告某题是否选中了某选项（渲染用）。
func (g *AskGroup) Selected(questionIndex, optionIndex int) bool {
	if questionIndex < 0 || questionIndex >= len(g.selected) {
		return false
	}
	_, ok := g.selected[questionIndex][optionIndex]
	return ok
}

// answers 生成答案：custom 只给**第一个没有选中任何选项**的问题。
//
// 已选了选项的问题不再附 custom，且 custom 只用一次——否则同一段文字会被塞进
// 每个问题的答案里，模型会以为用户对每个问题都这么答。
func (g *AskGroup) answers(custom string) []tools.QuestionAnswer {
	pending := strings.TrimSpace(custom)
	out := make([]tools.QuestionAnswer, 0, len(g.Questions))
	for index, question := range g.Questions {
		selected := make([]string, 0, len(question.Options))
		if index < len(g.selected) {
			for optionIndex := range g.selected[index] {
				if optionIndex < len(question.Options) {
					selected = append(selected, question.Options[optionIndex].Label)
				}
			}
			// map 遍历顺序随机：按选项顺序输出，答案才稳定可断言
			selected = sortByOptionOrder(question, selected)
		}
		answer := tools.QuestionAnswer{ID: question.ID, Selected: selected}
		switch {
		case g.CustomActive(index):
			// 该题此刻以手写答案作答（此时 selected 必为空）
			answer.Custom = g.Custom(index)
		case len(selected) == 0 && pending != "":
			// 兼容输入框作答：文字给第一个既没选也没写的问题
			answer.Custom = pending
			pending = ""
		}
		out = append(out, answer)
	}
	return out
}

// sortByOptionOrder 按选项在问题里的顺序排列选中的 label。
func sortByOptionOrder(question tools.QuestionSpec, selected []string) []string {
	if len(selected) <= 1 {
		return selected
	}
	ordered := make([]string, 0, len(selected))
	for _, option := range question.Options {
		for _, label := range selected {
			if label == option.Label {
				ordered = append(ordered, label)
				break
			}
		}
	}
	return ordered
}

// Output 生成写回工具结果的文本（模型可见）。
//
// 与 Rust 版 AskGroup::output 完全一致，三种情形：
//   - 跳过：`{"answers":[],"dismissed":true}`
//   - 提交：正常的 answers
//   - 输入文字但所有问题都已选中：文字放进 note（不静默丢弃）
func (g *AskGroup) Output(action AskAction, custom string) string {
	switch action {
	case AskDismiss:
		return `{"answers":[],"dismissed":true}`
	case AskCustom:
		answers := g.answers(custom)
		if hasCustom(answers) || strings.TrimSpace(custom) == "" {
			return answersJSON(answers)
		}
		// 全部已选：把用户的话作为 note 交给模型
		return fmt.Sprintf(`{"answers":%s,"note":%s}`, answersJSONInner(answers), jsonString(strings.TrimSpace(custom)))
	default:
		return answersJSON(g.answers(""))
	}
}

func hasCustom(answers []tools.QuestionAnswer) bool {
	for _, answer := range answers {
		if answer.Custom != "" {
			return true
		}
	}
	return false
}

func answersJSON(answers []tools.QuestionAnswer) string {
	return tools.AnswersToOutput(answers)
}

// answersJSONInner 返回 {"answers": ...} 里的数组部分。
func answersJSONInner(answers []tools.QuestionAnswer) string {
	full := tools.AnswersToOutput(answers)
	start := strings.Index(full, "[")
	end := strings.LastIndex(full, "]")
	if start < 0 || end < start {
		return "[]"
	}
	return full[start : end+1]
}

// jsonString 把字符串编码成 JSON 字符串字面量。
func jsonString(text string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, r := range text {
		switch r {
		case '"':
			builder.WriteString(`\"`)
		case '\\':
			builder.WriteString(`\\`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		default:
			builder.WriteRune(r)
		}
	}
	builder.WriteByte('"')
	return builder.String()
}
