package uiapp

import (
	"encoding/json"
	"strings"

	"simple-agent/internal/host"
)

// /subagent 命令：设置**派生出去的子会话**用哪个模型。
//
// 为什么要有它：子会话默认继承父会话的模型（那是刻意的 —— 它是替父会话
// 干活的），但"轻活派给便宜模型"是很常见的诉求。让用户在会话里用一条
// 命令定下来，比让 agent 在工具参数里决定用哪个模型花钱要合理。
//
// 命令**不改界面状态就完事**：设置属于执行端（它才知道有哪些模型、
// 也负责回写档案）。展示端（远程/Android）因此也能用同一条路径。
//
// 用法：
//
//	/subagent              查看当前设置
//	/subagent model <名>   设置
//	/subagent model off    清除（回到跟随本会话）
//
// **没有"弹列表"这一档**：模型列表本来就要靠执行端给，为了它多做一个
// 选择器界面并不划算 —— 不带参数时把可选值**打出来**就够了，用户照着敲。

// runSubagent 是 `/subagent` 命令的入口。
func runSubagent(a *App, args []string) string {
	session := a.Current()
	if session == nil {
		return "没有打开的会话"
	}
	if len(args) == 0 {
		return subagentStatus(a, session)
	}
	switch strings.ToLower(args[0]) {
	case "model", "m":
		if len(args) < 2 {
			return subagentModelHelp(a, session)
		}
		target := strings.Join(args[1:], " ")
		if target == "off" || target == "none" || target == "-" {
			target = ""
		}
		return a.setSubagentModel(session, target)
	default:
		return "未知子命令：" + args[0] + "\n" + subagentUsage()
	}
}

// subagentUsage 返回用法说明。
func subagentUsage() string {
	return "用法：\n" +
		"/subagent              查看当前设置\n" +
		"/subagent model <名称> 设置子会话模型\n" +
		"/subagent model off    恢复为跟随本会话"
}

// subagentStatus 显示当前设置。
func subagentStatus(a *App, session *Session) string {
	if model := session.SubagentModel(); model != "" {
		return "子会话模型：" + model + "\n" +
			"（不设置时子会话跟随本会话的模型与档位）"
	}
	sel := session.Selection()
	return "子会话模型：跟随本会话（" + a.modelLabel(sel.ProviderID, sel.Model) + "）\n" +
		"用 /subagent model <名称> 可改成别的模型"
}

// subagentModelHelp 列出可选模型（用户照着敲）。
//
// 只列**当前 provider** 下的模型：换 provider 意味着换 Key/端点，
// 那是另一个层面的决定，不该混在"这个会话派生的子任务用什么模型"里。
func subagentModelHelp(a *App, session *Session) string {
	sel := session.Selection()
	var models []string
	for _, choice := range a.providers() {
		if choice.ProviderID == sel.ProviderID {
			models = choice.Models
			break
		}
	}
	if len(models) == 0 {
		return "当前没有可选的模型（配置里没有给这个 provider 配模型列表）"
	}
	var builder strings.Builder
	builder.WriteString("用法：/subagent model <名称>（或 off 恢复跟随本会话）\n可选：")
	for _, model := range models {
		builder.WriteString("\n- ")
		builder.WriteString(a.modelLabel(sel.ProviderID, model))
		if label := a.modelLabel(sel.ProviderID, model); label != model {
			builder.WriteString("（" + model + "）")
		}
	}
	return builder.String()
}

// setSubagentModel 把设置提交给执行端（校验与落盘都在那边）。
//
// **必须走命令**：模型列表、校验、档案回写都在执行端，展示端只负责提交。
// 远程展示端下改本机内存是没意义的（那份状态执行端根本看不到）。
func (a *App) setSubagentModel(session *Session, model string) string {
	payload, err := json.Marshal(host.TextPayload{Text: model})
	if err != nil {
		return "设置没发出去：" + err.Error()
	}
	_, err = a.host.Submit(host.Command{
		SessionID: session.ID, Kind: "setSubagentModel", Payload: payload,
	})
	if err != nil {
		return "设置没发出去：" + err.Error()
	}
	// 命令是异步的（Deferred/远程）：校验结果由执行端的提示带回来，
	// 这里只能确认"已经提交"。
	if model == "" {
		return "子会话模型已恢复为跟随本会话"
	}
	return "子会话将使用模型 " + a.modelLabel(session.Selection().ProviderID, model)
}