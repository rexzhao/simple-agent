package host

import (
	"fmt"
	"strings"

	"simple-agent/internal/providers"
	"simple-agent/internal/tools"
)

// 子会话：由一个会话通过 new_session 工具派生出来的新会话。
//
// 与普通新建会话的差别只有三点：
//  1. 档案里记 parent_id（侧栏据此折叠显示，默认不展开）
//  2. 继承**父会话**的模型与工作区（而不是用全局默认）
//  3. 创建后立刻把 prompt 发出去，让它自己跑起来
//
// 没有引入"子会话类型"之类的机制：子会话就是普通会话，只是多了一条
// 来源记录。删除父会话、归档、压缩等等行为因此都不需要特殊处理。

// NewChildSession 在父会话所在的同一项目里新建一个子会话。
func (l *Local) NewChildSession(parent *Session, title, prompt string) (string, error) {
	if parent == nil {
		return "", fmt.Errorf("没有当前会话")
	}
	if parent.ProjectID == "" {
		return "", fmt.Errorf(
			"当前会话不属于任何项目（没有项目档案），无法新建子会话。" +
				"子会话的关系要记在项目档案里，没有档案就没有归属。")
	}
	title = strings.TrimSpace(title)
	if title == "" {
		// 空标题给个默认名：侧栏靠标题分辨会话，空字符串会显示成空白行。
		title = "子会话"
		if l.cfg.Tr != nil {
			if tr := l.cfg.Tr("session.childDefault"); tr != "" {
				title = tr
			}
		}
	}

	// 用父会话的工作区、模型与档位：子会话是"替父会话干活"的，
	// 换了模型或工作区会让结果对不上。
	//
	// **除非父会话用 /subagent 指定了别的模型**：那时按指定值来，
	// 档位留空交由 newSessionWith 回落到新模型的默认档位
	// （父会话的档位在新模型上可能不存在）。
	selection, effort := parent.Selection(), parent.Effort()
	if model := parent.SubagentModel(); model != "" {
		selection = providers.Selection{ProviderID: selection.ProviderID, Model: model}
		effort = parent.SubagentEffort()
	}
	child := l.newSessionWith(sessionSpec{
		Workspace: parent.Workspace,
		Title:     title,
		Selection: selection,
		Effort:    effort,
		ParentID:  parent.ID,
	})

	// prompt 非空就立刻开跑：子会话的价值就是"接手这件事"，
	// 让它空转等用户手动发消息没有意义。
	if strings.TrimSpace(prompt) != "" {
		if result := child.send(SendPayload{Text: prompt}); result.Error != "" {
			return child.ID, fmt.Errorf(
				"子会话已创建（id=%s），但发送初始任务失败：%s", child.ID, result.Error)
		}
	}
	return child.ID, nil
}

// sessionSpec 是新建会话的参数。
//
// 单独一个结构而不是给 NewSession 加一堆参数：现有调用点很多，
// 而派生会话是明确的少数派，独立的入口更清楚。
type sessionSpec struct {
	Workspace string
	Title     string
	// Selection 是模型选择（零值表示用全局默认）。
	Selection providers.Selection
	// Effort 是思考档位（空表示沿用模型配置）。
	Effort string
	// ParentID 非空表示这是派生会话。
	ParentID string
}

// toolsSessionHost 让"以某个会话为父"的创建操作满足 tools.SessionHost。
//
// 单独一层而不是直接在 Session 上加方法：工具层的语义是"以**当前会话**
// 为父联系创建一个子会话"，而 Session 的方法拿不到"父"这个角色信息。
type toolsSessionHost struct {
	local  *Local
	parent *Session
}

// NewChildSession 实现 tools.SessionHost。
func (t toolsSessionHost) NewChildSession(title, prompt string) (string, error) {
	return t.local.NewChildSession(t.parent, title, prompt)
}

var _ tools.SessionHost = toolsSessionHost{}

// attachSessionTools 给会话挂上"新建会话"的能力。
//
// **只挂给顶层会话**（没有 parent 的）：子会话不能再派生子会话。
//
// 为什么禁止：允许的话派生子会话就形成了一棵树，而树里天然可能出现
// 环（A 派 B、B 派 A），侧栏的折叠逻辑要额外处理这种脏数据。既然
// "子任务再拆子任务"本来就不是这个功能的目标（那属于子会话自己的工作
// 安排），直接在能力层禁掉更简单，也省掉一整类边界情况。
//
// 与 attachSubagent 分开调用：父子通讯要求有项目档案（关系记在档案里），
// 这里还多一条"只有顶层会话能派生"的限制。
func (l *Local) attachSessionTools(session *Session) {
	if session == nil || session.toolCtx == nil {
		return
	}
	if session.ProjectID == "" {
		return // 不属于任何项目：无法确定子会话该建在哪
	}
	if session.parentID != "" {
		return // 子会话不再派生（避免出现环与过深的层级）
	}
	session.toolCtx.Sessions = toolsSessionHost{local: l, parent: session}
}
