package uiapp

import (
	"github.com/egoist/mygo/ui"

	"simple-agent/internal/conversation"
	"simple-agent/internal/timeline"
)

// 过程组与提示用的图标，全部搬自 deepseek-harness 的 `ui-primitives/src/icons`
// （MIT，Copyright (c) 2026 DeepSeek，见 THIRD_PARTY_NOTICES.md）。
//
// 对照关系（dsh 的 `ChatGroupSeat.tsx` 里 `PROCESS_ICONS`）：
//
//	thinking → IconThinkOutlineRegular      read/readImage → IconBrowseOutlineRegular
//	search   → IconSearchOutlineRegular     write/edit     → IconEditOutlineRegular
//	commands → IconApiOutlineRegular        code           → IconCodeOutlineRegular
//	web*     → IconGlobeOutlineRegular      subagents      → IconAgentPresetOutlineRegular
//	plan     → IconEditOutline（dsh 用 ListPen，未搬）  questions → IconQuestionOutlineRegular
//	tools    → IconSparkleRegular
const (
	iconThink = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1" xmlns="http://www.w3.org/2000/svg">
<path d="M10.2854 5.71481C12.9673 8.39663 14.1182 11.5938 12.8562 12.8559C11.5942 14.1179 8.39706 12.9669 5.71518 10.2851C3.03333 7.60323 1.88236 4.40608 3.14441 3.14403C4.40644 1.882 7.6036 3.03297 10.2854 5.71481Z"/>
<path d="M10.2854 10.2851C7.6036 12.9669 4.40644 14.1179 3.14441 12.8559C1.88236 11.5938 3.03333 8.39663 5.71518 5.71481C8.39706 3.03297 11.5942 1.882 12.8562 3.14403C14.1182 4.40608 12.9673 7.60323 10.2854 10.2851Z"/>
</svg>`

	iconBrowse = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1" xmlns="http://www.w3.org/2000/svg">
<path d="M4.9375 5.90295H11.0625"/>
<path d="M4.9375 9.02991H8.27841"/>
<path d="M12.5 1.32617C13.3039 1.32617 14 1.95171 14 2.77637V13.2246C13.9996 14.0489 13.3036 14.6738 12.5 14.6738H3.5C2.69637 14.6738 2.00042 14.0489 2 13.2246V2.77637C2 1.95171 2.69613 1.32617 3.5 1.32617H12.5Z"/>
</svg>`

	iconEdit = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1" xmlns="http://www.w3.org/2000/svg">
<path d="M8.85596 2.69971H4.19971C3.37141 2.69971 2.69992 3.37146 2.69971 4.19971V11.8003C2.69992 12.6285 3.37141 13.3003 4.19971 13.3003H11.8003C12.6283 13.2999 13.3001 12.6283 13.3003 11.8003V7.89893H14.3003V11.8003C14.3001 12.8333 13.5084 13.6758 12.4945 13.7407L12.3 13.7443H4.19971C3.09027 13.7443 2.18959 12.8802 2.04803 11.8364L2.03302 11.6347V4.19971C2.03302 3.09027 2.89717 2.18959 3.94098 2.04803L4.19971 2.03302H8.85596V2.69971Z"/>
<path d="M7.7849 8.23878L13.888 2.13574"/>
</svg>`

	iconCommands = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1" xmlns="http://www.w3.org/2000/svg">
<path d="M3 4L7 8L3 12"/>
<path d="M9 12H13"/>
</svg>`

	iconCode = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1" xmlns="http://www.w3.org/2000/svg">
<path d="M6.27612 1.5L4.52612 14.5"/>
<path d="M11.4739 1.5L9.72388 14.5"/>
<path d="M2.39868 5.5H14.0681"/>
<path d="M1.93188 10.5H13.6013"/>
</svg>`

	iconGlobe = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1" xmlns="http://www.w3.org/2000/svg">
<path d="M7.99986 14.0887C11.3626 14.0887 14.0886 11.3627 14.0886 7.99998C14.0886 4.63727 11.3626 1.91125 7.99986 1.91125C4.63715 1.91125 1.91113 4.63727 1.91113 7.99998C1.91113 11.3627 4.63715 14.0887 7.99986 14.0887Z"/>
<path d="M2.34619 8H13.6538"/>
<path d="M7.99976 14.0889C9.23509 14.0889 10.1743 11.3629 10.1743 8.00006C10.1743 4.63739 9.23509 1.91138 7.99976 1.91138"/>
<path d="M7.99973 14.0889C6.76445 14.0889 5.8252 11.3629 5.8252 8.00006C5.8252 4.63739 6.76445 1.91138 7.99973 1.91138"/>
</svg>`

	iconSparkle = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1" xmlns="http://www.w3.org/2000/svg">
<path d="M5.875 3C5.875 6.33333 7.54167 8 10.875 8C7.54167 8 5.875 9.66667 5.875 13C5.875 9.66667 4.20833 8 0.875 8C4.20833 8 5.875 6.33333 5.875 3Z"/>
<path d="M12.375 1.55823C12.375 3.39156 13.2917 4.30823 15.125 4.30823C13.2917 4.30823 12.375 5.22489 12.375 7.05823C12.375 5.22489 11.4583 4.30823 9.625 4.30823C11.4583 4.30823 12.375 3.39156 12.375 1.55823Z"/>
</svg>`

	iconQuestion = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1" xmlns="http://www.w3.org/2000/svg">
<path d="M8 14.5C11.5899 14.5 14.5 11.5899 14.5 8C14.5 4.41015 11.5899 1.5 8 1.5C4.41015 1.5 1.5 4.41015 1.5 8C1.5 11.5899 4.41015 14.5 8 14.5Z"/>
<path d="M8 10.7416V11.7416"/>
</svg>`

	iconAgent = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1" xmlns="http://www.w3.org/2000/svg">
<path d="M7.99136 5.28105C8.87501 5.28105 9.59136 4.56471 9.59136 3.68105C9.59136 2.7974 8.87501 2.08105 7.99136 2.08105C7.1077 2.08105 6.39136 2.7974 6.39136 3.68105C6.39136 4.56471 7.1077 5.28105 7.99136 5.28105Z"/>
<path d="M3.94009 12.9417C4.82374 12.9417 5.54009 12.2254 5.54009 11.3417C5.54009 10.458 4.82374 9.7417 3.94009 9.7417C3.05643 9.7417 2.34009 10.458 2.34009 11.3417C2.34009 12.2254 3.05643 12.9417 3.94009 12.9417Z"/>
<path d="M12.0851 12.9417C12.9688 12.9417 13.6851 12.2254 13.6851 11.3417C13.6851 10.458 12.9688 9.7417 12.0851 9.7417C11.2015 9.7417 10.4851 10.458 10.4851 11.3417C10.4851 12.2254 11.2015 12.9417 12.0851 12.9417Z"/>
</svg>`

	// 提示图标：dsh 的 IconWarningOutline 是圆圈叹号，info 是圆圈 i。
	iconWarning = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1" xmlns="http://www.w3.org/2000/svg">
<path d="M8 14.5C11.5899 14.5 14.5 11.5899 14.5 8C14.5 4.41015 11.5899 1.5 8 1.5C4.41015 1.5 1.5 4.41015 1.5 8C1.5 11.5899 4.41015 14.5 8 14.5Z"/>
<path d="M8 4.29199V9.79199"/>
<path d="M8 10.708V11.708"/>
</svg>`

	iconInfo = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1" xmlns="http://www.w3.org/2000/svg">
<path d="M7 13.5757C10.6304 13.5757 13.5757 10.6304 13.5757 7C13.5757 3.36954 10.6304 0.424561 7 0.424561C3.36954 0.424561 0.424561 3.36954 0.424561 7C0.424561 10.6304 3.36954 13.5757 7 13.5757Z"/>
<path d="M7.6127 3.18921V4.55986H6.38735V3.18921H7.6127Z"/>
<path d="M7.6127 5.68921V10.8109H6.38735V5.68921H7.6127Z"/>
</svg>`
)

var (
	iconThinkSVG    = ui.MustParseSVG([]byte(iconThink))
	iconBrowseSVG   = ui.MustParseSVG([]byte(iconBrowse))
	iconEditSVG     = ui.MustParseSVG([]byte(iconEdit))
	iconCommandsSVG = ui.MustParseSVG([]byte(iconCommands))
	iconCodeSVG     = ui.MustParseSVG([]byte(iconCode))
	iconGlobeSVG    = ui.MustParseSVG([]byte(iconGlobe))
	iconSparkleSVG  = ui.MustParseSVG([]byte(iconSparkle))
	iconQuestionSVG = ui.MustParseSVG([]byte(iconQuestion))
	iconAgentSVG    = ui.MustParseSVG([]byte(iconAgent))
	iconWarningSVG  = ui.MustParseSVG([]byte(iconWarning))
	iconInfoSVG     = ui.MustParseSVG([]byte(iconInfo))
)

// activityIcon 把活动分类映射到 dsh 的图标（对齐 `PROCESS_ICONS`）。
func activityIcon(kind processActivity) *ui.SVG {
	switch kind {
	case timeline.ActivityThinking:
		return iconThinkSVG
	case timeline.ActivityRead, timeline.ActivityReadImage, timeline.ActivityWebFetch:
		return iconBrowseSVG
	case timeline.ActivitySearch:
		return iconSearchSVG
	case timeline.ActivityWrite, timeline.ActivityEdit, timeline.ActivityPlan:
		// dsh 的 plan 用 ListPen，这里暂时复用编辑图标（差别很小）
		return iconEditSVG
	case timeline.ActivityCommands:
		return iconCommandsSVG
	case timeline.ActivityCode:
		return iconCodeSVG
	case timeline.ActivityWebSearch:
		return iconGlobeSVG
	case timeline.ActivitySubagents:
		return iconAgentSVG
	case timeline.ActivityQuestions:
		return iconQuestionSVG
	default:
		return iconSparkleSVG
	}
}

// noticeIcon 把通知级别映射到图标（颜色由调用方按级别给）。
func noticeIcon(level conversation.NoticeLevel) *ui.SVG {
	switch level {
	case conversation.NoticeError, conversation.NoticeWarn:
		return iconWarningSVG
	default:
		return iconInfoSVG
	}
}
