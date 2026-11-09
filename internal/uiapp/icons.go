package uiapp

import "github.com/egoist/mygo/ui"

// 界面图标：SVG 直接搬自 deepseek-harness 的 `ui-primitives/src/icons`
// （MIT，Copyright (c) 2026 DeepSeek，见 THIRD_PARTY_NOTICES.md）。
//
// 说明：dsh 是 React 组件（stroke="currentColor"），这里转成静态 SVG 字符串，
// 用 `ui.MustParseSVG` 解析、`ui.Icon` 绘制；颜色由调用方 `TextColor` 决定。
// 规则（方案 §0.5）：图标只用这套 dsh 的，不自己画。
const (
	// iconNewChat 是「新建会话」（dsh IconNewChatOutlineRegular，1px 描边）。
	iconNewChat = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1" xmlns="http://www.w3.org/2000/svg">
<path d="M2.37091 11.2501C1.58745 9.89288 1.32067 8.29835 1.61969 6.76006C1.91872 5.22177 2.76342 3.8433 3.99826 2.87846C5.2331 1.91362 6.77494 1.42737 8.33988 1.50925C9.90482 1.59113 11.3875 2.23562 12.5149 3.32406C13.6425 4.41269 14.3387 5.87206 14.4754 7.4334C14.612 8.99474 14.18 10.5529 13.2587 11.8209C12.3375 13.0888 10.9891 13.9813 9.46194 14.3337C8.18691 14.628 6.85895 14.5294 5.64989 14.0605C5.1712 13.8748 4.76962 13.4932 4.26534 13.3967C3.67413 13.2835 2.95257 13.5598 2.03794 14.3337"/>
<path d="M8 5V11"/>
<path d="M5 8H11"/>
</svg>`

	// iconPlus 是「附件」（dsh IconPlusOutlineRegular）。
	iconPlus = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1" xmlns="http://www.w3.org/2000/svg">
<path d="M8 2V14"/>
<path d="M2 8H14"/>
</svg>`

	// iconEllipsis 是「更多」菜单（dsh IconEllipsisOutline）。
	iconEllipsis = `<svg viewBox="0 0 16 16" fill="none" xmlns="http://www.w3.org/2000/svg">
<path d="M3 9C3.55228 9 4 8.55228 4 8C4 7.44772 3.55228 7 3 7C2.44772 7 2 7.44772 2 8C2 8.55228 2.44772 9 3 9Z" fill="currentColor"/>
<path d="M8 9C8.55228 9 9 8.55228 9 8C9 7.44772 8.55228 7 8 7C7.44772 7 7 7.44772 7 8C7 8.55228 7.44772 9 8 9Z" fill="currentColor"/>
<path d="M13 9C13.5523 9 14 8.55228 14 8C14 7.44772 13.5523 7 13 7C12.4477 7 12 7.44772 12 8C12 8.55228 12.4477 9 13 9Z" fill="currentColor"/>
</svg>`

	// iconChevronDown 是「可展开」的向下箭头（dsh IconChevronDownOutlineRegular）。
	iconChevronDown = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1" xmlns="http://www.w3.org/2000/svg">
<path d="M3.5 6L8 10.5L12.5 6"/>
</svg>`

	// iconSend 是圆形发送按钮里的箭头（dsh IconSendOutlineRegular）。
	iconSend = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1" xmlns="http://www.w3.org/2000/svg">
<path d="M6.97211 1.94476C7.55785 1.35914 8.50767 1.35919 9.09343 1.94476L13.921 6.77228L13.2138 7.47939L8.38632 2.65187C8.19108 2.45682 7.87443 2.45677 7.67922 2.65187L2.74397 7.58711L2.03687 6.88L6.97211 1.94476Z"/>
<path d="M7.97571 14.5732L8.02421 2.34139"/>
</svg>`

	// iconSearch 是搜索（dsh IconSearchOutlineRegular）。
	iconSearch = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1" xmlns="http://www.w3.org/2000/svg">
<path d="M6.58727 11.8586C9.55061 11.8586 11.9529 9.45637 11.9529 6.49304C11.9529 3.5297 9.55061 1.12744 6.58727 1.12744C3.62394 1.12744 1.22168 3.5297 1.22168 6.49304C1.22168 9.45637 3.62394 11.8586 6.58727 11.8586Z"/>
<path d="M10.2991 10.3933L14.7783 14.8725"/>
</svg>`

	// iconPanelLeft 是侧栏开关（dsh IconPanelLeftOutlineRegular）。
	iconPanelLeft = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1" xmlns="http://www.w3.org/2000/svg">
<path d="M13.5 1.5H2.5C1.94772 1.5 1.5 1.94772 1.5 2.5V13.5C1.5 14.0523 1.94772 14.5 2.5 14.5H13.5C14.0523 14.5 14.5 14.0523 14.5 13.5V2.5C14.5 1.94772 14.0523 1.5 13.5 1.5Z"/>
<path d="M5.5 1.5V14.5"/>
</svg>`

	// iconCopy 是「复制」（dsh IconCopyOutlineRegular：1px 描边的双叠矩形）。
	iconCopy = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1" xmlns="http://www.w3.org/2000/svg">
<rect x="1.52075" y="4.07373" width="10.3932" height="10.3932" rx="2" stroke="currentColor"/>
<path d="M11.9792 1.53296C13.36 1.53296 14.4792 2.65225 14.4792 4.03296V9.42847C14.4792 10.3756 13.9521 11.1987 13.1755 11.6228V10.3298C13.3652 10.0787 13.4792 9.7674 13.4792 9.42847V4.03296C13.4792 3.20453 12.8077 2.53296 11.9792 2.53296H6.58374C6.27966 2.53301 5.99684 2.6235 5.7605 2.77905H4.42358C4.85652 2.03463 5.66056 1.53304 6.58374 1.53296H11.9792Z" fill="currentColor"/>
</svg>`

	// iconCheck 是「已复制」（dsh IconCheckOutlineRegular）。
	iconCheck = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1" xmlns="http://www.w3.org/2000/svg">
<path d="M2.5 8.5L6 12L13.5 4.5"/>
</svg>`
)

// 解析后的图标（进程内只解析一次；解析失败说明常量写错了，直接不显示）。
var (
	iconNewChatSVG     = ui.MustParseSVG([]byte(iconNewChat))
	iconPanelLeftSVG   = ui.MustParseSVG([]byte(iconPanelLeft))
	iconSearchSVG      = ui.MustParseSVG([]byte(iconSearch))
	iconPlusSVG        = ui.MustParseSVG([]byte(iconPlus))
	iconEllipsisSVG    = ui.MustParseSVG([]byte(iconEllipsis))
	iconSendSVG        = ui.MustParseSVG([]byte(iconSend))
	iconChevronDownSVG = ui.MustParseSVG([]byte(iconChevronDown))
	iconCopySVG        = ui.MustParseSVG([]byte(iconCopy))
	iconCheckSVG       = ui.MustParseSVG([]byte(iconCheck))
)

// iconButton 渲染一个图标按钮（悬停有底色 + 悬停提示 + 无障碍名）。
//
// label 同时用作：**无障碍名**（读屏与测试定位）和 **Tooltip 文案**（鼠标悬停时的提示）。
// 图标按钮没有可见文字，两者缺一不可：只有 Label 的话，用户把鼠标放上去什么都看不到。
func iconButton(c *ui.Context, svg *ui.SVG, label string, size, pad float32) bool {
	theme := c.Theme()
	button := ui.Row(c).Padding(pad).Radius(6).Cursor(ui.CursorPointer).
		Label(label).Tooltip(label)
	if button.Hovered() {
		button.Background(dsh.SurfaceHover)
	}
	button.Children(func() {
		ui.Icon(c, svg).Size(size, size).TextColor(theme.TextMuted)
	})
	return button.Clicked()
}
