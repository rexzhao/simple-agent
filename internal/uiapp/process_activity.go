package uiapp

import (
	"simple-agent/internal/timeline"
)

// 过程组的分类与标题逻辑已经搬到 `internal/timeline`（Android 展示端要用同一份）。
// 这里只保留界面自己的映射：分类 → 图标。

// processActivity 是过程组的活动分类（与 timeline.Activity 对应）。
type processActivity = timeline.Activity

// toolArgSummary 生成工具调用的一行参数摘要（实现在 timeline）。
func toolArgSummary(data map[string]any) string { return timeline.ToolArgSummary(data) }

// processGroupTitle 生成过程组标题（实现在 timeline）。
func (a *App) processGroupTitle(refs []partRef, streaming bool) string {
	return timeline.GroupTitle(refs, streaming, a.lang)
}

// groupActivity 返回这一组当前（或最后一次）活动的分类，供标题前的图标使用。
func groupActivity(refs []partRef, streaming bool) processActivity {
	return timeline.GroupActivity(refs, streaming)
}
