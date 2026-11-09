// Package tools 是本地工具链（对齐 deepseek-harness 的工具名、参数与语义）。
//
// 已实现：read / write / edit / glob / grep / working_directory / todo_write /
// ask_user_question / bash / pwsh。
//
// 安全边界：所有路径限制在工作区（或会话 cwd）内，绝对路径与 `..` 逃逸一律拒绝；
// 词法校验之外还要 lstat + EvalSymlinks，否则工作区里的一个链接（克隆的仓库、
// 解压的 zip 都可能带进来）就能把读写带到外面。`bash`/`pwsh` 需显式开启。
//
// 与 Rust 版的差异（G5）：Run 接 context.Context，取消即中止——shell 命令杀整棵
// 进程树，文件工具在读写前检查 ctx。工具层不 import providers/ui：参数声明由
// driver 转成各协议的格式。
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
)

// QuestionOption 是一个可选项。
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// QuestionSpec 是一个待回答的问题。
type QuestionSpec struct {
	ID          string           `json:"id"`
	Question    string           `json:"question"`
	Header      string           `json:"header,omitempty"`
	Options     []QuestionOption `json:"options,omitempty"`
	MultiSelect bool             `json:"multiSelect,omitempty"`
}

// QuestionAnswer 是用户对单个问题的回答。
//
// Selected 不用 omitempty：Rust 版（以及 deepseek-harness 的 schema）总是给出
// selected 数组，模型据此判断「用户没有选任何选项」，缺字段会被当成两种含义。
type QuestionAnswer struct {
	ID       string   `json:"id"`
	Selected []string `json:"selected"`
	Custom   string   `json:"custom,omitempty"`
}

// AnswersToOutput 把回答序列化成模型可见的工具结果文本。
func AnswersToOutput(answers []QuestionAnswer) string {
	data, err := json.Marshal(map[string]any{"answers": answers})
	if err != nil {
		return `{"answers":[]}`
	}
	return string(data)
}

// SkippedAnswers 构造「用户跳过全部问题」的回答（全部 selected 为空）。
func SkippedAnswers(questions []QuestionSpec) []QuestionAnswer {
	answers := make([]QuestionAnswer, 0, len(questions))
	for _, q := range questions {
		answers = append(answers, QuestionAnswer{ID: q.ID})
	}
	return answers
}

// TodoStatus 是任务状态。
type TodoStatus string

const (
	TodoPending    TodoStatus = "pending"
	TodoInProgress TodoStatus = "in_progress"
	TodoCompleted  TodoStatus = "completed"
)

func (s TodoStatus) glyph() string {
	switch s {
	case TodoCompleted:
		return "[x]"
	case TodoInProgress:
		return "[~]"
	default:
		return "[ ]"
	}
}

// TodoItem 是一条任务。
type TodoItem struct {
	Content string     `json:"content"`
	Status  TodoStatus `json:"status"`
}

// SessionState 是会话级可变状态（cwd、任务清单）。
type SessionState struct {
	cwd   string
	todos []TodoItem
}

// ToolContext 是一次会话内共享的工具上下文。
type ToolContext struct {
	// workspace 是工作区根目录，路径解析的最终边界。
	workspace string
	// AllowShell 控制是否允许执行命令。
	AllowShell bool
	// Vision 表示当前模型是否接受图片输入（read_image 据此决定能否工作）。
	Vision bool

	mu    sync.Mutex
	state SessionState
}

// NewToolContext 构造上下文（cwd 初始为工作区根）。
func NewToolContext(workspace string, allowShell bool) *ToolContext {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		abs = workspace
	}
	return &ToolContext{
		workspace:  abs,
		AllowShell: allowShell,
		state:      SessionState{cwd: abs},
	}
}

// Workspace 返回工作区根目录。
func (c *ToolContext) Workspace() string { return c.workspace }

// Cwd 返回当前会话目录。
func (c *ToolContext) Cwd() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state.cwd
}

func (c *ToolContext) setCwd(dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state.cwd = dir
}

// Todos 返回任务清单快照。
func (c *ToolContext) Todos() []TodoItem {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]TodoItem, len(c.state.todos))
	copy(out, c.state.todos)
	return out
}

func (c *ToolContext) setTodos(todos []TodoItem) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state.todos = todos
}

// ToolReturn 是工具执行结果：正常输出，或向用户提问。
//
// 两者互斥：Ask 非空时 Output 为空。UI 渲染提问面板，用户作答后答案作为工具
// 结果回填。故意不用「一个字符串字段 + 兜底」的写法——那会让提问被静默当成
// 空输出，`.IsEmpty()` 之类的判断会把「等待回答」误判成「没有输出」。
type ToolReturn struct {
	Output string
	Ask    []QuestionSpec
	// Images 是工具返回的图片（read_image 用）。调用方负责写入 blob 存储，
	// 并把引用挂进工具结果，模型才能在请求里真正"看到"它。
	Images []ImageOutput
}

// IsAsk 报告是否需要向用户提问。
func (r ToolReturn) IsAsk() bool { return len(r.Ask) > 0 }

// OutputOf 构造输出结果。
func OutputOf(text string) ToolReturn { return ToolReturn{Output: sanitizeUTF8(text)} }

// AskOf 构造提问结果。
func AskOf(questions []QuestionSpec) ToolReturn { return ToolReturn{Ask: questions} }

// ToolDef 是一个可执行工具。
type ToolDef struct {
	// Name 是模型可见的工具名（与 harness 一致）。
	Name string
	// Description 是给模型看的说明：这个工具**是什么**。
	Description string
	// Guidance 是面向模型的**使用指导**：什么时候该用它、要注意什么。
	//
	// 与 Description 分开（对齐 deepseek-harness 的 tool 段落）：schema 说
	// "这个工具是干什么的"，guidance 说"别用 shell 绕过去、改之前先读"。
	// 这些是真正影响行为的约束，塞进 Description 会让每个工具的 schema 变长，
	// 而它们只在**工具可见时**才该出现在系统提示里——由 SystemPrompt 负责。
	//
	// 空表示这个工具不需要额外指导（只有 Description 就够）。
	Guidance string
	// Parameters 是 JSON Schema。
	Parameters any
	// Run 执行工具；ctx 取消应尽快返回 ctx.Err()。
	Run func(ctx context.Context, args Args, tc *ToolContext) (ToolReturn, error)
}

// Args 是解析后的工具参数。
type Args map[string]any

// Str 取必填字符串参数。
func (a Args) Str(key string) (string, error) {
	value, ok := a[key].(string)
	if !ok {
		return "", fmt.Errorf("缺少 %s 参数", key)
	}
	return value, nil
}

// OptStr 取可选字符串参数（空串视为未提供）。
func (a Args) OptStr(key string) string {
	value, _ := a[key].(string)
	if value == "" {
		return ""
	}
	for _, r := range value {
		if r != ' ' && r != '\t' && r != '\n' && r != '\r' {
			return value
		}
	}
	return ""
}

// Bool 取可选布尔参数。
func (a Args) Bool(key string) bool {
	value, _ := a[key].(bool)
	return value
}

// Int 取可选整数参数（兼容 float64 / 字符串形式）。
func (a Args) Int(key string) (int, bool) {
	switch v := a[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n), true
		}
	case string:
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n, true
		}
	}
	return 0, false
}

// Items 取对象数组参数。
func (a Args) Items(key string) ([]map[string]any, error) {
	raw, ok := a[key].([]any)
	if !ok {
		return nil, fmt.Errorf("%s 必须是数组", key)
	}
	out := make([]map[string]any, 0, len(raw))
	for i, item := range raw {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s[%d] 必须是对象", key, i)
		}
		out = append(out, obj)
	}
	return out, nil
}

// ToolOutcome 是一次工具调用的结果（对应 ToolResult part 的字段）。
type ToolOutcome struct {
	ID      string
	Name    string
	Output  string
	IsError bool
	// Images 是工具返回的图片引用，已由调用方写入 blob 存储（`blob:sha256:<hex>`）。
	// 工具层不碰存储：写盘、去重、失败降级都由 uiapp 那边负责。
	Images []ToolImageRef
	// RawImages 是工具刚返回、还没落盘的图片字节（只在内存里传一手）。
	RawImages []ImageOutput
	// Pending 非空表示需要用户回答（此时 Output 为空，作答后回填）。
	Pending []QuestionSpec
}

// ToolImageRef 是工具结果里的一张图片引用。
type ToolImageRef struct {
	// Ref 是 `blob:sha256:<hex>`。
	Ref       string
	MediaType string
	Width     int
	Height    int
}

// IsPending 报告是否在等待用户回答。
func (o ToolOutcome) IsPending() bool { return len(o.Pending) > 0 }

// Call 是一次待执行的工具调用。
type Call struct {
	ID   string
	Name string
	Args Args
}

// Builtin 返回内置工具（顺序即 system 提示中的顺序）。
//
// shell 工具只在 allowShell 时注册：注册了却不给用，会让模型反复试错。
func Builtin(allowShell bool) []*ToolDef {
	list := []*ToolDef{
		readTool(),
		readImageTool(),
		writeTool(),
		editTool(),
		globTool(),
		grepTool(),
		todoWriteTool(),
		workingDirectoryTool(),
		askUserQuestionTool(),
	}
	if allowShell {
		list = append(list, bashTool())
		if isWindows() {
			list = append(list, pwshTool())
		}
	}
	return list
}

// Find 按名查找工具。
func Find(list []*ToolDef, name string) (*ToolDef, bool) {
	for _, tool := range list {
		if tool.Name == name {
			return tool, true
		}
	}
	return nil, false
}

// ExecuteAll 顺序执行工具调用。
//
// 未知工具与执行错误都返回一条错误结果（而不是中断整批）：模型能看到失败原因
// 并自行纠正。ctx 取消时后续调用直接标记为已中止。
func ExecuteAll(ctx context.Context, list []*ToolDef, tc *ToolContext, calls []Call) []ToolOutcome {
	outcomes := make([]ToolOutcome, 0, len(calls))
	for _, call := range calls {
		outcomes = append(outcomes, executeOne(ctx, list, tc, call))
	}
	return outcomes
}

func executeOne(ctx context.Context, list []*ToolDef, tc *ToolContext, call Call) ToolOutcome {
	base := ToolOutcome{ID: call.ID, Name: call.Name}

	if err := ctx.Err(); err != nil {
		base.Output = "已中止：用户停止了生成。"
		base.IsError = true
		return base
	}

	tool, ok := Find(list, call.Name)
	if !ok {
		base.Output = fmt.Sprintf("未知工具：%s。可用工具见 system 提示。", call.Name)
		base.IsError = true
		return base
	}

	result, err := tool.Run(ctx, call.Args, tc)
	if err != nil {
		base.Output = err.Error()
		// ctx 取消导致的错误统一文案（区分工具自身错误）
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			base.Output = "已中止：用户停止了生成。"
		}
		base.IsError = true
		return base
	}

	if result.IsAsk() {
		base.Pending = result.Ask
		return base
	}
	base.Output = result.Output
	base.RawImages = result.Images
	if base.Output == "" && len(base.RawImages) == 0 {
		base.Output = "（无输出）"
	}
	return base
}

// schema 构造 JSON Schema。
func schema(properties map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   required,
	}
}

// displayPath 相对工作区显示（统一用 `/`，跨平台一致）。
func displayPath(tc *ToolContext, path string) string {
	rel, err := filepath.Rel(tc.workspace, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

// walkFiles 遍历目录（跳过构建产物与 VCS 目录），visit 收到文件路径；
// visit 返回 false 时停止遍历。
func walkFiles(root string, visit func(path string) bool) {
	var walk func(dir string) bool
	walk = func(dir string) bool {
		entries, err := readDirSorted(dir)
		if err != nil {
			return true
		}
		for _, entry := range entries {
			if skipDirs[entry.Name()] && entry.IsDir() {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if entry.IsDir() {
				if !walk(path) {
					return false
				}
				continue
			}
			// 符号链接按文件处理：沙箱在 resolve 阶段已校验过目标
			if !entry.IsDir() && !visit(path) {
				return false
			}
		}
		return true
	}
	walk(root)
}

// readDirSorted 按名字排序读取目录（遍历结果稳定，测试可断言）。
func readDirSorted(dir string) ([]dirEntry, error) {
	entries, err := osReadDir(dir)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}
