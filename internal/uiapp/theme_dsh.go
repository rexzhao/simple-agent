package uiapp

import "github.com/egoist/mygo/ui"

// dsh 设计令牌（浅色）→ mygo Theme。
//
// 色值、字体栈、尺寸全部来自 deepseek-harness（MIT，见 THIRD_PARTY_NOTICES.md）：
//   - packages/client/ui-theme/src/styles/design-platform.css（浅色段）
//   - packages/client/ui-theme/src/styles/base.css（字体栈）
//
// 规则（方案 §0.5）：**后续每一期只能用这里的令牌，不要再自己定数值**。
//
// 主题应用策略：**始终使用这套浅色令牌**（用户决定，不跟随系统深色模式）。
// `themeFor` 是唯一的开关位置。

// dsh 的语义别名（浅色），命名沿用 dsh 的变量名，便于与源码对照。
//
// 用一个结构体装起来（而不是一堆包级变量）：先定义、后使用的令牌很多，
// 包级变量会被 staticcheck 的 U1000 判为未使用，而结构体字段不会。
// 后续阶段（圆角、代码块、菜单…）直接 `dsh.CodeBlock` 这样取。
type dshTokenSet struct {
	// 背景层
	BgBase        ui.Color // --dsw-alias-bg-base
	BgModule      ui.Color // --dsw-alias-bg-module-platform (bluish-60)
	SidebarFill   ui.Color // --dsw-specific-sidebar-fill (bluish-50)
	SurfaceHover  ui.Color // --dsw-alias-interactive-bg-hover-solid (bluish-75)
	SurfaceActive ui.Color // --dsw-specific-sidebar-nav-item-active (bluish-100)

	// 文字
	LabelPrimary   ui.Color // bluish-1000
	LabelSecondary ui.Color // bluish-700
	LabelTertiary  ui.Color // bluish-600
	LabelCaption   ui.Color // bluish-400

	// 品牌与气泡
	//
	// 注意：dsh 的 --dsw-alias-brand-primary 其实是近黑（bluish-1000），
	// 蓝色那套叫 brand-primary-new-color。主按钮与强调色是两个概念（§0.5.1）。
	BrandBlue   ui.Color // brand-primary-new-color / link (deepseek-500)
	BrandBlueHi ui.Color // deepseek-450
	BrandBlueLo ui.Color // deepseek-600
	ButtonSolid ui.Color // dsh 的主按钮（近黑 bluish-1000）
	Bubble      ui.Color // --dsw-specific-bubble (deepseek-50)
	BubbleHi    ui.Color // --dsw-specific-bubble-highlight (deepseek-200)
	Inverse     ui.Color // tooltip-bg (bluish-850)
	Scrollbar   ui.Color // scrollbar-bg-l1 (neutral-200)
	ScrollbarHi ui.Color // scrollbar-hover-l1 (neutral-300)

	// 状态色
	Error   ui.Color // state-error-primary (red-600)
	Success ui.Color // state-success-primary (green-500)
	Warning ui.Color // state-warn-primary (amber-500)

	// 边框四级：l1..l4 是同一颜色的不同不透明度
	BorderL1 ui.Color
	BorderL2 ui.Color
	BorderL3 ui.Color
	BorderL4 ui.Color

	// markdown / 代码块
	CodeBlock  ui.Color // markdown-code-block (bluish-50)
	InlineCode ui.Color // markdown-inline-code (neutral-50)

	// 任务清单面板（dsh TodoPanel）
	//
	// `--dsw-specific-menu` = rgba(248,249,250,.58)。mygo 的颜色没有 alpha 合成
	// 之外的"毛玻璃"，这里取**合成到白底后的等效实色**（.58 × (248,249,250)
	// + .42 × 255 = (251,251,252)），避免与背后内容叠加时出现色差。
	TodoMenuSurface ui.Color
	// StateIdle 是 `--dsw-alias-state-idle-primary` = neutral-300（212,212,212）。
	StateIdle ui.Color
}

// dsh 是浅色令牌表（唯一来源）。
var dsh = dshTokenSet{
	BgBase:        ui.Hex("#FFFFFF"),
	BgModule:      ui.Hex("#F5F6F7"),
	SidebarFill:   ui.Hex("#F9FAFB"),
	SurfaceHover:  ui.Hex("#F1F3F5"),
	SurfaceActive: ui.Hex("#EBEEF2"),

	LabelPrimary:   ui.Hex("#0F1115"),
	LabelSecondary: ui.Hex("#61666B"),
	LabelTertiary:  ui.Hex("#81858C"),
	LabelCaption:   ui.Hex("#ADB2B8"),

	BrandBlue:   ui.RGB(65, 118, 230),
	BrandBlueHi: ui.RGB(86, 134, 254),
	BrandBlueLo: ui.RGB(72, 104, 178),
	ButtonSolid: ui.RGB(15, 17, 21),
	Bubble:      ui.Hex("#EDF3FE"),
	BubbleHi:    ui.Hex("#D3E2FF"),
	Inverse:     ui.Hex("#2C2C2E"),
	Scrollbar:   ui.Hex("#E5E5E5"),
	ScrollbarHi: ui.Hex("#D4D4D4"),

	Error:   ui.Hex("#EC1313"),
	Success: ui.Hex("#22C55E"),
	Warning: ui.Hex("#F59E0B"),

	BorderL1: ui.RGBA(0, 0, 0, 0.04),
	BorderL2: ui.RGBA(0, 0, 0, 0.10),
	BorderL3: ui.RGBA(0, 0, 0, 0.12),
	BorderL4: ui.RGBA(0, 0, 0, 0.16),

	CodeBlock:  ui.Hex("#F9FAFB"),
	InlineCode: ui.Hex("#FAFAFA"),

	TodoMenuSurface: ui.Hex("#FBFBFC"),
	StateIdle:       ui.RGB(212, 212, 212),
}

// 字体栈（dsh base.css 原样）。mygo 的 Font 接受逗号分隔的族列表。
const (
	// dshTextFontStack 是正文栈：Windows 上落到 Segoe UI / 微软雅黑。
	dshTextFontStack = "-apple-system, BlinkMacSystemFont, 'Segoe UI', 'PingFang SC', " +
		"'Hiragino Sans GB', 'Microsoft YaHei', 'Helvetica Neue', Helvetica, Arial, sans-serif"
	// dshCodeFontStack 是代码栈：Windows 上落到 Consolas。
	dshCodeFontStack = "'SF Mono', 'JetBrains Mono', 'Fira Code', Consolas, " +
		"'Liberation Mono', Menlo, Courier, 'PingFang SC', 'Microsoft YaHei'"
)

// dshLightTheme 按 dsh 的浅色令牌构造一个 mygo Theme。
//
// 字段映射说明（mygo 只有这些槽位，dsh 的变量更多）：
//   - Background ← bg-base；Surface ← 控件面（白）；SurfaceHover/Pressed ← 交互 hover/active；
//   - Border ← border-l2（l1 太浅，控件边框会看不见；更浅的分隔线直接用 dshBorderL1）；
//   - Text ← label-primary；TextMuted ← label-secondary；
//   - Accent ← **品牌蓝**（dsh 的强调色）；注意 dsh 的**主按钮**是近黑（`dshNeutral900`），
//     所以需要"近黑主按钮"的地方不能靠 Accent，得自己画（见 P0.0 提交说明）；
//   - Selection ← 用户气泡底色（现有气泡直接用它）；
//   - Inverse ← tooltip 底（bluish-850）。
func dshLightTheme() *ui.Theme {
	return &ui.Theme{
		Dark:           false,
		Background:     dsh.BgBase,
		Surface:        dsh.BgBase,
		SurfaceHover:   dsh.SurfaceHover,
		SurfacePressed: dsh.SurfaceActive,
		Border:         dsh.BorderL2,
		Text:           dsh.LabelPrimary,
		TextMuted:      dsh.LabelSecondary,
		Accent:         dsh.BrandBlue,
		AccentHover:    dsh.BrandBlueHi,
		AccentPressed:  dsh.BrandBlueLo,
		AccentText:     dsh.BgBase,
		Danger:         dsh.Error,
		Warning:        dsh.Warning,
		Success:        dsh.Success,
		Selection:      dsh.Bubble,
		Focus:          dsh.BrandBlue,
		Inverse:        dsh.Inverse,
		InverseText:    dsh.BgBase,
		Scrollbar:      dsh.Scrollbar,
		ScrollbarWidth: 6,
		Radius:         8,
		Spacing:        4,
		FontSize:       13,
		Font:           dshTextFontStack,
	}
}

// themeFor 返回本帧要用的主题。
//
// **界面始终是 dsh 浅色**（用户决定）：不跟随系统深色模式切换，系统是深色时也用浅色。
// 所以这里无条件返回浅色令牌——这是"一处开关"：以后若要支持深色，就在这里按
// current.Dark 分支。
func themeFor(current *ui.Theme) *ui.Theme {
	_ = current
	return dshLightTheme()
}
