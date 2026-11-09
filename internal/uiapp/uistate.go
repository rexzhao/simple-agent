package uiapp

import (
	"log"

	"simple-agent/internal/uistate"
)

// 本机界面偏好（ui-state.json）的读写。
//
// 归属：**每台客户端自己的**偏好，存在系统默认应用配置目录（见 uistate 包），
// 不写执行端的 data_dir。因此展示端（--connect）也有自己的偏好，
// 不需要数据目录锁，也不会与执行端抢文件。
//
// 容错：读失败一律按默认值继续（偏好丢了是小事，拦住启动是大事）。

// loadUIState 读取本机偏好；路径为空时只保留默认值。
func (a *App) loadUIState() {
	a.uiStatePath = a.opts.UIStatePath
	if a.uiStatePath == "" {
		return
	}
	state, err := uistate.Load(a.uiStatePath)
	if err != nil {
		// Load 已经回了默认值，这里只是让用户能从日志看出"为什么宽度变了"
		log.Printf("界面偏好：%v", err)
	}
	if state.ChatContentWidth > 0 {
		a.contentWidthPreference = float32(state.ChatContentWidth)
	}
}

// saveUIState 把当前偏好写回磁盘。
//
// 只在**拖动松手**时调用（对齐 dsh 的 onCommit）：拖动过程中每帧写盘既慢
// 又没意义，中途崩了也该以最后一次完整拖动为准。
//
// 失败只在**第一次**打日志：拖宽度是高频操作，如果环境原因一直存不下去
// （只读目录、被策略锁住），每次松手刷一行日志会把控制台淹掉。
func (a *App) saveUIState() {
	if a.uiStatePath == "" {
		return
	}
	state := uistate.Default()
	if a.contentWidthPreference > 0 {
		state.ChatContentWidth = int(a.contentWidthPreference)
	}
	if err := uistate.Save(a.uiStatePath, state); err != nil {
		if !a.uiStateSaveFailed {
			a.uiStateSaveFailed = true
			// 写不进去不该影响使用：界面照常，只是下次启动恢复不了宽度
			log.Printf("界面偏好：%v（本次运行不再重复提示）", err)
		}
		return
	}
	a.uiStateSaveFailed = false
}
