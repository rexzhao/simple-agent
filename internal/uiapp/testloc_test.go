package uiapp_test

// 测试定位用的**可见文案**集中在这里。
//
// 目的：界面文案可能随设计调整（尤其"英文界面"的需求在讨论中），把 60+ 处字面量
// 收到一处，改文案时只改这里；测试逻辑不依赖具体文字。
//
// 注意（先别定死组织方式）：用户正在确认"英文界面"的范围（全英文 / 双语 / 跟随系统）。
// 如果最后要做双语，这些常量会变成 locale 表里的查表，届时只改这个文件即可——
// 所以这里**只放字符串常量**，不引入任何 i18n 结构，避免过早绑定设计。
const (
	locSend          = "发送"      // 输入卡发送/插话按钮
	locStop          = "停止"      // 停止生成
	locSubmit        = "提交"      // 提问面板提交
	locDismiss       = "跳过"      // 提问面板跳过
	locNewSession    = "新建会话"    // 侧栏新建会话（图标按钮的无障碍名）
	locSidebarHide   = "收起侧边栏"   // 收起侧栏（图标按钮的无障碍名）
	locSidebarShow   = "打开侧边栏"   // 展开侧栏（图标按钮的无障碍名）
	locCompact       = "压缩上下文"   // 「更多」菜单里的压缩项
	locMore          = "更多"      // 头部「更多」菜单
	locModel         = "选择模型"    // 输入卡右下的模型 chip（无障碍名）
	locSteer         = "插话"      // 忙时发送按钮的无障碍名
	locSessionMore   = "会话操作"    // 侧栏会话行悬停出现的「⋯」
	locSidebarToggle = "展开或收起项目" // 侧栏项目行的三角
	locDeleteSession = "删除会话"    // 头部「更多」菜单里的删除项
	locRename        = "改名"      // 会话行改名
	locDelete        = "删除"      // 会话行删除
	locDeleteOK      = "确认删除"    // 二次确认
	locConfirm       = "确定"      // 通用确认
	locCancel        = "取消"      // 通用取消
	locCopyMessage   = "复制这条消息"  // 用户气泡的复制图标（无障碍名）
	locCopyAnswer    = "复制回答"    // 回合结束行的复制图标（无障碍名）
	locCopiedAnswer  = "已复制回答"   // 上者点击后的反馈态无障碍名
	locRenameInput   = "重命名输入"   // 重命名输入框的无障碍名
	locComposer      = "消息输入"    // 输入框的无障碍名
	locDivider       = "Divider" // Split 分隔条的无障碍名（mygo 提供）
)

// 英文界面的对应文案（TestEnglishUI 用）。
const (
	locSendEN       = "Send"
	locStopEN       = "Stop"
	locNewSessionEN = "New session"
)
